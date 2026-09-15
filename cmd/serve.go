package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
)

func init() {
	serveCmd.Flags().StringVar(&serveOpts.transport, "transport", transportStdio, "Transport: stdio or streamable-http")
	serveCmd.Flags().StringVar(&serveOpts.httpAddr, "http-addr", ":8080", "Listen address for the streamable-http transport")
	rootCmd.AddCommand(serveCmd)
}

const (
	transportStdio          = "stdio"
	transportStreamableHTTP = "streamable-http"
	// mcpEndpoint is the path the streamable HTTP transport is served on;
	// /healthz and /readyz answer the Kubernetes probes next to it.
	mcpEndpoint = "/mcp"
	// shutdownGrace bounds how long a stopping server waits for in-flight
	// requests -- a sweep can run for minutes, but a terminating pod gets
	// seconds.
	shutdownGrace = 10 * time.Second
)

var serveOpts struct {
	transport string
	httpAddr  string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start an MCP server exposing sweep and mark as tools",
	Long: `Start a Model Context Protocol (MCP) server.
The server exposes a "sweep" tool that mirrors the sweep CLI command,
returning structured JSON results instead of terminal output, and a "mark"
tool that mirrors the mark CLI command.

Transports:
  stdio            JSON-RPC over stdin/stdout, for a local MCP client that
                   starts marge itself (default)
  streamable-http  the MCP Streamable HTTP transport on --http-addr, with the
                   endpoint at ` + mcpEndpoint + ` and liveness/readiness probes
                   at /healthz and /readyz; this is what the Helm chart runs`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mcpServer := newMCPServer()
		switch serveOpts.transport {
		case transportStdio:
			return server.ServeStdio(mcpServer)
		case transportStreamableHTTP:
			return serveHTTP(cmd.Context(), mcpServer, serveOpts.httpAddr, cmd.ErrOrStderr())
		default:
			return fmt.Errorf("unknown transport %q: use %s or %s", serveOpts.transport, transportStdio, transportStreamableHTTP)
		}
	},
}

// newMCPServer builds the MCP server with the sweep and mark tools; the
// transport is chosen by the caller.
func newMCPServer() *server.MCPServer {
	mcpServer := server.NewMCPServer(
		"marge",
		version,
		server.WithToolCapabilities(true),
	)

	mcpServer.AddTool(sweepTool(), handleSweep)

	mcpServer.AddTool(
		mcp.NewTool("mark",
			mcp.WithDescription("Record a failed AI rescue attempt on a PR by posting a machine-readable ai-rescue marker comment. Subsequent sweeps surface the marker so the operator knows a rescue was already attempted. The marker records the head SHA and a fingerprint of the PR diff: it goes stale when the PR content changes (new version, pushed fix) but survives a Renovate rebase that leaves the diff unchanged."),
			mcp.WithString("pr_url",
				mcp.Required(),
				mcp.Description("Pull request URL (https://github.com/OWNER/REPO/pull/NUMBER)"),
			),
			mcp.WithString("outcome",
				mcp.Description("Rescue outcome (default: \"failed\")"),
				mcp.Enum("failed", "blocked"),
			),
			mcp.WithString("reason",
				mcp.Description("Short explanation of why the rescue did not succeed"),
			),
			mcp.WithString("tool",
				mcp.Description("Name of the tool/agent that attempted the rescue (default: \"ai\")"),
			),
		),
		handleMark,
	)

	return mcpServer
}

// httpHandler routes the MCP endpoint and the probe paths. Everything else
// is a 404, so a misconfigured client gets a clear answer instead of an MCP
// error.
func httpHandler(mcpServer *server.MCPServer) (http.Handler, *server.StreamableHTTPServer) {
	streamable := server.NewStreamableHTTPServer(mcpServer, server.WithEndpointPath(mcpEndpoint))
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	mux.Handle(mcpEndpoint, streamable)
	return mux, streamable
}

// serveHTTP serves the MCP server over Streamable HTTP until ctx is done or
// SIGINT/SIGTERM arrives, then drains in-flight requests for shutdownGrace.
// No write timeout is set on purpose: a sweep tool call streams for as long
// as the sweep runs.
func serveHTTP(ctx context.Context, mcpServer *server.MCPServer, addr string, log io.Writer) error {
	handler, streamable := httpHandler(mcpServer)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		_, _ = fmt.Fprintf(log, "marge %s serving MCP over streamable HTTP on %s%s\n", version, addr, mcpEndpoint)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving on %s: %w", addr, err)
	case <-ctx.Done():
	}

	_, _ = fmt.Fprintln(log, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	streamable.CloseSessions(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

// sweepTool declares the sweep tool and its arguments. parseSweepRequest
// reads exactly these arguments; the serve tests keep the two in step.
func sweepTool() mcp.Tool {
	return mcp.NewTool("sweep",
		mcp.WithDescription("Sweep bot PRs (Renovate, Align files, Herald, Dependabot): classify every open bot PR of a team or query scope, approve and squash-merge the eligible green ones, "+
			"label each with bot-prs-sweep/<class>, and hold majors and unreadable updates for a person. A pending or unreported required check is a wait, never a bypass; a failing security check is never merged past. "+
			"Returns structured JSON: summary counts plus merged, security_failures, action_required, stale, refreshed, cancelled, retried, waiting, obsolete, ci_unavailable, ci_no_verdict and skipped lists, and repositories_failed for repositories that could not be listed. "+
			"A failing PR whose head is behind its base branch and whose every failing check is green on the base branch head is classified as stale "+
			"(the failure was fixed on the base branch after the PR's last build) and listed under stale, not action_required; "+
			"the refresh action updates such branches from their base so CI re-runs (they are then listed under refreshed). "+
			"A failing PR whose every failing check is a CircleCI build that CircleCI itself auto-cancelled (a newer pipeline on the branch, a redundant workflow) "+
			"is classified as cancelled and listed under cancelled, not action_required: there is no verdict on the code yet; "+
			"the retry action reruns their workflow from its failed jobs on the same commit (they are then listed under retried). "+
			"A failing check that established nothing about the code is excluded from action_required too and listed under ci_no_verdict, with the remedy in its detail: "+
			"a cancelled job (rerun it), or a CircleCI pipeline refused because setup workflows are disabled for the repository (a human must change the project setting). "+
			"A security check in that shape is not a finding and is never listed under security_failures. "+
			"A failing or conflicted bot PR that a sibling with a higher version of the same dependency replaces, or whose diff changes nothing that executes "+
			"(a pinned GitHub Actions SHA whose trailing version comment is all that moved), is listed under obsolete with a reason of superseded or no_op: "+
			"it wants closing, not fixing, so it is not in action_required. A green PR still merges. "+
			"Rescue tooling should act on action_required only, and skip entries whose rescue object is not stale: "+
			"a prior automated rescue already failed on exactly this change (rebased: true means the branch was merely rebased since, the attempt still stands)."),
		mcp.WithString("query",
			mcp.Description("Narrow the sweep the way `marge [query]` does. Without repos/repos_file the text becomes part of the GitHub search, "+
				"so it can be free text matched against the PR (a dependency name such as \"typescript\") or search qualifiers (\"repo:my-org/my-repo\"). "+
				"With repos or repos_file it keeps only the listed repositories whose org/repo contains the text (case-insensitive)."),
		),
		mcp.WithString("org",
			mcp.Description("GitHub organization or user to limit the sweep to"),
		),
		mcp.WithString("repos_file",
			mcp.Description("Path to a file listing org/repo entries (one per line; blank lines and # comments are ignored) to scan for bot PRs instead of searching GitHub. "+
				"When repos is given too, both lists are merged and duplicates dropped."),
		),
		mcp.WithArray("repos",
			mcp.Description("Explicit list of repos (org/repo format) to scan for bot PRs instead of searching GitHub. "+
				"When repos_file is given too, both lists are merged and duplicates dropped."),
			mcp.WithStringItems(),
		),
		mcp.WithBoolean("merge_auto",
			mcp.Description("Also merge PRs that have auto-merge enabled (default: false)"),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Show what would be done without making changes (default: false). Stale PRs are still classified, but not refreshed."),
		),
		mcp.WithString("team",
			mcp.Description("Sweep the repositories of this team, read from repositories/team-<name>.yaml in the team-file repository (giantswarm/github, or $MARGE_TEAM_FILE_REPO). Mutually exclusive with query, org, repos and repos_file."),
		),
		mcp.WithString("actions",
			mcp.Description("Comma-separated sweep steps to run, in fixed order: classify, approve, merge, refresh, retry, mark (default: all). refresh updates stale branches from their base; retry reruns the CircleCI workflow of auto-cancelled builds on the same head from its failed jobs, falling back to a single-build retry (needs CIRCLECI_CLI_TOKEN or ~/.circleci/cli.yml); mark writes markers and evidence comments."),
		),
		mcp.WithString("security_patterns",
			mcp.Description("Comma-separated case-insensitive substrings added to the built-in list that flags failing CI checks as security-related"),
		),
	)
}

// sweepRequest is what one call of the sweep tool asks for: the search
// inputs and the processing options, defaulting like the sweep CLI command.
// The repos_file argument lands in Opts.ReposFile, where the CLI flag of the
// same meaning lives.
type sweepRequest struct {
	Query string
	Repos []string
	Opts  RunOptions
}

// resolveScope returns what the sweep covers: the repositories of the
// request merged with those of the scope, without duplicates, and the
// policy the scope resolves to. No repository at all means no restriction,
// so the PRs come from the GitHub search.
func (r sweepRequest) resolveScope(ctx context.Context, client *github.Client) (policy.Scope, error) {
	scope, err := r.Opts.resolveScope(ctx, client)
	if err != nil {
		return policy.Scope{}, err
	}
	scope.Repos = mergeRepos(r.Repos, scope.Repos)
	return scope, nil
}

// mergeRepos joins repository lists into one, trimmed and without
// duplicates, keeping the first spelling of each entry in order. GitHub
// treats owner and repository names case-insensitively, so the comparison
// does too. It returns nil when the lists hold no entry.
func mergeRepos(lists ...[]string) []string {
	seen := make(map[string]bool)
	var merged []string
	for _, list := range lists {
		for _, repo := range list {
			repo = strings.TrimSpace(repo)
			key := strings.ToLower(repo)
			if repo == "" || seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, repo)
		}
	}
	return merged
}

// parseSweepRequest reads the arguments declared by sweepTool. Quiet is
// always set: stdout is the MCP stdio transport, so no table, plain-text
// results or progress chatter may be written; the JSON result carries the
// same data.
func parseSweepRequest(request mcp.CallToolRequest) (sweepRequest, error) {
	actions, err := process.ParseActions(request.GetString("actions", ""))
	if err != nil {
		return sweepRequest{}, err
	}
	req := sweepRequest{
		Query: request.GetString("query", ""),
		Repos: request.GetStringSlice("repos", nil),
		Opts: RunOptions{
			DryRun:           request.GetBool("dry_run", false),
			MergeAuto:        request.GetBool("merge_auto", false),
			Quiet:            true,
			Team:             request.GetString("team", ""),
			Actions:          actions,
			Org:              request.GetString("org", ""),
			ReposFile:        request.GetString("repos_file", ""),
			SecurityPatterns: request.GetString("security_patterns", ""),
		},
	}
	if req.Opts.Team != "" && (req.Query != "" || len(req.Repos) > 0 || req.Opts.Org != "" || req.Opts.ReposFile != "") {
		return sweepRequest{}, errors.New("team is mutually exclusive with query, org, repos and repos_file")
	}
	return req, nil
}

// SweepResult is the structured JSON output returned by the sweep MCP tool.
type SweepResult struct {
	Summary          SweepSummary   `json:"summary"`
	Merged           []SweepPREntry `json:"merged,omitempty"`
	SecurityFailures []SweepPREntry `json:"security_failures,omitempty"`
	ActionRequired   []SweepPREntry `json:"action_required,omitempty"`
	// Stale lists failing PRs whose head is behind the base branch and whose
	// every failing check is green on the base branch head: the failure was
	// fixed on the base branch after the PR's last build. The remedy is a
	// branch refresh (the refresh action), not a rescue, so they are excluded from
	// action_required.
	Stale []SweepPREntry `json:"stale,omitempty"`
	// Refreshed lists stale PRs whose branch was updated from its base in
	// this run. CI is running again; the next sweep decides what they are.
	Refreshed []SweepPREntry `json:"refreshed,omitempty"`
	// Cancelled lists failing PRs whose every failing check is a CircleCI
	// build that CircleCI itself auto-cancelled: there is no verdict on the
	// code yet. The remedy is a retry (the retry action), not a rescue, so
	// they are excluded from action_required. A build cancelled behind a
	// newer head is listed too but never retried: the new head's own build
	// is the verdict.
	Cancelled []SweepPREntry `json:"cancelled,omitempty"`
	// Retried lists cancelled PRs whose workflow was rerun on the same
	// commit in this run. CI is running again; the next sweep decides.
	Retried []SweepPREntry `json:"retried,omitempty"`
	// CIUnavailable lists PRs whose CI could not run because a GitHub Actions
	// budget / spending-limit block prevented every job from starting. These
	// are NOT failures: the remedy is to raise or await the Actions budget,
	// so they are reported separately and excluded from action_required.
	CIUnavailable []SweepPREntry `json:"ci_unavailable,omitempty"`
	// CINoVerdict lists PRs whose every failing check established nothing
	// about the code: a cancelled job, or a pipeline a CircleCI project
	// setting refuses. These are NOT failures, and a security check in this
	// shape is NOT a finding, so they are excluded from action_required and
	// security_failures. Each entry's detail names its own remedy.
	CINoVerdict []SweepPREntry `json:"ci_no_verdict,omitempty"`
	// Obsolete lists bot PRs that are not worth fixing: a sibling PR carries
	// a higher version of the same dependency, or the diff changes nothing
	// that executes. Each entry's reason says which. The remedy is to close
	// them, so they are excluded from action_required.
	Obsolete []SweepPREntry `json:"obsolete,omitempty"`
	// Waiting lists PRs whose required checks have not all reported: a
	// required context is pending or was never reported. The sweep never
	// merges past a required check; the next sweep decides.
	Waiting []SweepPREntry `json:"waiting,omitempty"`
	Skipped []SweepPREntry `json:"skipped,omitempty"`
	// RepositoriesFailed lists the repositories whose PRs could not be
	// listed, so a partial sweep is visible as such.
	RepositoriesFailed []SweepRepoFailure `json:"repositories_failed,omitempty"`
}

// SweepRepoFailure names a repository the sweep could not list.
type SweepRepoFailure struct {
	Repo  string `json:"repo"`
	Error string `json:"error"`
}

// SweepSummary contains aggregate counts from the sweep.
//
// Failed and SecurityFailures are disjoint: Failed counts only the
// non-security failure entries, so consumers can use
// Failed + SecurityFailures to get the total number of action-required
// PRs without double-counting.
type SweepSummary struct {
	Total            int `json:"total"`
	Merged           int `json:"merged"`
	Failed           int `json:"failed"`
	SecurityFailures int `json:"security_failures"`
	// CIUnavailable counts PRs whose CI could not run because of a GitHub
	// Actions budget block. It is disjoint from Failed and SecurityFailures.
	CIUnavailable int `json:"ci_unavailable"`
	// CINoVerdict counts PRs whose every failing check established nothing
	// about the code. It is disjoint from Failed and SecurityFailures.
	CINoVerdict int `json:"ci_no_verdict"`
	// Stale counts failing PRs whose failure is already fixed on the base
	// branch (see SweepResult.Stale); Refreshed counts the stale PRs whose
	// branch was updated in this run. Both are disjoint from Failed.
	Stale     int `json:"stale"`
	Refreshed int `json:"refreshed"`
	// Cancelled counts failing PRs whose failing builds CircleCI itself
	// cancelled (see SweepResult.Cancelled); Retried counts the cancelled
	// PRs whose builds were retried in this run. Both are disjoint from
	// Failed.
	Cancelled int `json:"cancelled"`
	Retried   int `json:"retried"`
	// Obsolete counts bot PRs that are not worth fixing, whether a
	// higher-version sibling replaced them or their diff changes nothing
	// that executes. Disjoint from Failed.
	Obsolete int `json:"obsolete"`
	// Waiting counts PRs whose required checks have not all reported.
	Waiting int `json:"waiting"`
	Skipped int `json:"skipped"`
}

// SweepPREntry represents a single PR in the sweep results.
type SweepPREntry struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Kind is the bot that authored the PR; UpdateType the size of the
	// update it carries; Label the bot-prs-sweep/<class> label that is on
	// the PR after the sweep, empty when nothing was written.
	Kind       string `json:"kind,omitempty"`
	UpdateType string `json:"update_type,omitempty"`
	Label      string `json:"label,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	AgeDays    int    `json:"age_days,omitempty"`
	// Rescue describes the most recent prior automated rescue attempt
	// found on the PR (an ai-rescue marker comment), if any. Consumers
	// dispatching rescue agents should skip entries with a non-stale
	// failed rescue and escalate them to a human instead.
	Rescue *SweepRescueInfo `json:"rescue,omitempty"`
	// Reason says why an obsolete PR is obsolete: "superseded" or "no_op".
	// Only set on entries in the obsolete list.
	Reason string `json:"reason,omitempty"`
	// Policy is the sweep policy this PR was decided under, so an outcome
	// explains itself without the reader resolving the files again.
	Policy *SweepPolicyInfo `json:"policy,omitempty"`
}

// SweepPolicyInfo is the JSON projection of a pr.Policy.
type SweepPolicyInfo struct {
	Sweep bool `json:"sweep"`
	// UpdateTypes lists the update types that merge when green, per bot PR
	// kind.
	UpdateTypes  map[string][]string `json:"update_types"`
	Schedule     bool                `json:"schedule"`
	Rescue       SweepRescuePolicy   `json:"rescue"`
	Concurrency  SweepConcurrency    `json:"concurrency"`
	ModelConfig  string              `json:"model_config,omitempty"`
	SlackChannel string              `json:"slack_channel,omitempty"`
	// Sources names the files that produced the policy, in the order they
	// were applied.
	Sources []string `json:"sources,omitempty"`
}

// SweepRescuePolicy is the rescue section of a resolved policy.
type SweepRescuePolicy struct {
	Enabled bool   `json:"enabled"`
	Timeout string `json:"timeout,omitempty"`
	Weekly  int    `json:"weekly"`
	// BudgetPerRescueUSD and BudgetWeeklyUSD are what the team declared.
	// BudgetEnforced says whether this build applies them; it is false
	// until the platform accepts a budget on a run and reports the cost of
	// a finished one.
	BudgetPerRescueUSD float64 `json:"budget_per_rescue_usd,omitempty"`
	BudgetWeeklyUSD    float64 `json:"budget_weekly_usd,omitempty"`
	BudgetEnforced     bool    `json:"budget_enforced"`
	Confirm            string  `json:"confirm,omitempty"`
}

// SweepConcurrency is the concurrency section of a resolved policy.
type SweepConcurrency struct {
	PerTeam int `json:"per_team"`
	PerRepo int `json:"per_repo"`
}

// policyInfo projects a resolved policy into its JSON shape.
func policyInfo(resolved *pr.Policy) *SweepPolicyInfo {
	if resolved == nil {
		return nil
	}
	types := make(map[string][]string, len(resolved.UpdateTypes))
	for kind, updateTypes := range resolved.UpdateTypes {
		names := make([]string, 0, len(updateTypes))
		for _, updateType := range updateTypes {
			names = append(names, string(updateType))
		}
		sort.Strings(names)
		types[string(kind)] = names
	}
	info := &SweepPolicyInfo{
		Sweep:       resolved.Sweep,
		UpdateTypes: types,
		Schedule:    resolved.Schedule,
		Rescue: SweepRescuePolicy{
			Enabled:            resolved.Rescue.Enabled,
			Weekly:             resolved.Rescue.Weekly,
			BudgetPerRescueUSD: resolved.Rescue.Budget.PerRescueUSD,
			BudgetWeeklyUSD:    resolved.Rescue.Budget.WeeklyUSD,
			BudgetEnforced:     pr.BudgetEnforced,
			Confirm:            string(resolved.Rescue.Confirm),
		},
		Concurrency:  SweepConcurrency{PerTeam: resolved.Concurrency.PerTeam, PerRepo: resolved.Concurrency.PerRepo},
		ModelConfig:  resolved.ModelConfig,
		SlackChannel: resolved.SlackChannel,
		Sources:      resolved.Sources,
	}
	if resolved.Rescue.Timeout > 0 {
		info.Rescue.Timeout = resolved.Rescue.Timeout.String()
	}
	return info
}

// SweepRescueInfo is the JSON projection of a pr.RescueMarker.
type SweepRescueInfo struct {
	Tool    string `json:"tool,omitempty"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	At      string `json:"at,omitempty"`
	// Stale is true when the PR content changed since the rescue attempt
	// -- the attempt no longer describes the current code and the PR is
	// fair game for another rescue.
	Stale bool `json:"stale"`
	// Rebased is true when the PR head moved since the rescue attempt but
	// the change did not (a Renovate rebase onto a newer base): the marker
	// still describes the current code and Stale is false.
	Rebased bool `json:"rebased"`
}

func handleSweep(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, err := parseSweepRequest(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	client, err := gh.NewClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("creating GitHub client: %v", err)), nil
	}

	scope, err := req.resolveScope(ctx, client)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	req.Opts.Policies = scope.Policies

	me, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("getting authenticated user: %v", err)), nil
	}
	login := me.GetLogin()

	found, err := searchPRs(ctx, client, req.Query, login, scope.Repos)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("searching PRs: %v", err)), nil
	}
	prs := filterByOrg(found.PRs, req.Opts.Org)

	status, err := processOnceWithStatus(ctx, client, login, prs, req.Opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("processing PRs: %v", err)), nil
	}

	result := buildSweepResult(status, found.Failed)

	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshaling results: %v", err)), nil
	}

	return mcp.NewToolResultText(string(jsonBytes)), nil
}

func buildSweepResult(status *pr.PRStatus, failed []repoFailure) SweepResult {
	counts := status.Summary()
	total := status.Len()
	securityEntries := status.SecurityFailedEntries()
	blockedEntries := status.BlockedEntries()

	result := SweepResult{
		Summary: SweepSummary{
			Total:            total,
			Merged:           counts.Merged,
			Failed:           counts.Failed - len(securityEntries),
			SecurityFailures: len(securityEntries),
			CIUnavailable:    counts.Blocked,
			CINoVerdict:      counts.NoVerdict,
			Stale:            counts.Stale,
			Refreshed:        counts.Refreshed,
			Cancelled:        counts.Cancelled,
			Retried:          counts.Retried,
			Obsolete:         counts.Obsolete,
			Waiting:          counts.Waiting,
			Skipped:          counts.Skipped,
		},
	}
	for _, f := range failed {
		result.RepositoriesFailed = append(result.RepositoriesFailed, SweepRepoFailure{Repo: f.Repo, Error: f.Err})
	}

	now := time.Now()
	toEntry := func(e pr.StatusEntry) SweepPREntry {
		entry := SweepPREntry{
			Owner:  e.PR.Owner,
			Repo:   e.PR.Repo,
			Number: e.PR.Number,
			Title:  e.PR.Title,
			URL:    e.PR.URL,
			Status: e.State.String(),
			Detail: e.Detail,
			Reason: string(e.ObsoleteReason),
			Kind:   string(e.Kind),
		}
		if e.UpdateType != "" {
			entry.UpdateType = string(e.UpdateType)
		}
		entry.Label = e.Label
		entry.Policy = policyInfo(e.Policy)
		if !e.PR.CreatedAt.IsZero() {
			entry.CreatedAt = e.PR.CreatedAt.UTC().Format(time.RFC3339)
			entry.AgeDays = pr.AgeDays(e.PR.CreatedAt, now)
		}
		if e.Rescue != nil {
			entry.Rescue = &SweepRescueInfo{
				Tool:    e.Rescue.Tool,
				Outcome: e.Rescue.Outcome,
				Reason:  e.Rescue.Reason,
				Stale:   e.Rescue.Stale,
				Rebased: e.Rescue.Rebased,
			}
			if !e.Rescue.At.IsZero() {
				entry.Rescue.At = e.Rescue.At.UTC().Format(time.RFC3339)
			}
		}
		return entry
	}

	for _, e := range status.MergedEntries() {
		result.Merged = append(result.Merged, toEntry(e))
	}

	for _, e := range securityEntries {
		result.SecurityFailures = append(result.SecurityFailures, toEntry(e))
	}

	for _, e := range blockedEntries {
		result.CIUnavailable = append(result.CIUnavailable, toEntry(e))
	}

	for _, e := range status.NoVerdictEntries() {
		result.CINoVerdict = append(result.CINoVerdict, toEntry(e))
	}

	for _, e := range status.ObsoleteEntries() {
		result.Obsolete = append(result.Obsolete, toEntry(e))
	}

	for _, e := range status.StaleEntries() {
		result.Stale = append(result.Stale, toEntry(e))
	}

	for _, e := range status.RefreshedEntries() {
		result.Refreshed = append(result.Refreshed, toEntry(e))
	}

	for _, e := range status.CancelledEntries() {
		result.Cancelled = append(result.Cancelled, toEntry(e))
	}

	for _, e := range status.RetriedEntries() {
		result.Retried = append(result.Retried, toEntry(e))
	}

	for _, e := range status.WaitingEntries() {
		result.Waiting = append(result.Waiting, toEntry(e))
	}

	for _, e := range status.ActionRequired() {
		if e.State == pr.StatusFailedSecurity {
			continue
		}
		result.ActionRequired = append(result.ActionRequired, toEntry(e))
	}

	for _, e := range status.SkippedEntries() {
		result.Skipped = append(result.Skipped, toEntry(e))
	}

	return result
}

func handleMark(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	prURL := request.GetString("pr_url", "")
	outcome := request.GetString("outcome", "failed")
	reason := request.GetString("reason", "")
	tool := request.GetString("tool", "ai")

	client, err := gh.NewClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("creating GitHub client: %v", err)), nil
	}

	marker, owner, repo, number, err := markRescue(ctx, client, prURL, outcome, reason, tool)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result := map[string]any{
		"owner":    owner,
		"repo":     repo,
		"number":   number,
		"outcome":  marker.Outcome,
		"tool":     marker.Tool,
		"head_sha": marker.HeadSHA,
		"at":       marker.At.Format(time.RFC3339),
	}
	if marker.PatchID != "" {
		result["patch_id"] = marker.PatchID
	}
	if marker.ChangeID != "" {
		result["change_id"] = marker.ChangeID
	}
	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshaling result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonBytes)), nil
}
