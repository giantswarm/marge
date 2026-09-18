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
	"sync"
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
		switch serveOpts.transport {
		case transportStdio:
			return server.ServeStdio(newMCPServer(gh.NewClient))
		case transportStreamableHTTP:
			return serveHTTP(cmd.Context(), newMCPServer(gh.NewCallerClient), serveOpts.httpAddr, cmd.ErrOrStderr())
		default:
			return fmt.Errorf("unknown transport %q: use %s or %s", serveOpts.transport, transportStdio, transportStreamableHTTP)
		}
	},
}

// clientFactory returns the GitHub client one tool call acts with, and is
// what separates the two transports. A stdio server was started by the
// person using it, so it acts with that person's own credential; a served
// one acts with the token its caller presented and with nothing else.
type clientFactory func(context.Context) (*github.Client, error)

// toolset holds what every tool handler needs. Each tool is an adapter: it
// reads its arguments, builds one sweepRequest and hands it to run, which is
// the same engine call the sweep command makes. No tool reaches past it and
// no tool knows what kind of client it got.
type toolset struct {
	newClient clientFactory
}

// newMCPServer builds the MCP server and its tools over newClient; the
// transport is chosen by the caller.
func newMCPServer(newClient clientFactory) *server.MCPServer {
	mcpServer := server.NewMCPServer(
		"marge",
		version,
		server.WithToolCapabilities(true),
	)

	tools := toolset{newClient: newClient}
	mcpServer.AddTool(listTool(), tools.handleList)
	mcpServer.AddTool(sweepTool(), tools.handleSweep)
	mcpServer.AddTool(remedyTool(), tools.handleRemedy)
	mcpServer.AddTool(markTool(), tools.handleMark)

	return mcpServer
}

// Every tool spells out all four annotations. mcp-go fills an unset hint
// with the specification's default, so a hint left out ships as a claim.
// openWorldHint is true throughout: every tool reads and writes GitHub,
// which is outside this server.
func listTool() mcp.Tool {
	return mcp.NewTool("list",
		mcp.WithDescription("Read-only. List the open bot PRs of a team or query scope with the classification, label, bot kind and age of each. "+
			"By default it reads back the classification the last sweep stored in the PR's marge/<class> label, so it reports what the last sweep decided, not what a sweep would decide now. "+
			"refresh: true decides every PR now instead: it is the sweep engine's classify step alone, on a dry run. "+
			"Neither reading approves, merges, refreshes, retries, remedies or labels anything, and neither writes a comment. "+
			"unclassified means no sweep has labelled that PR, not that the PR needs nothing: call again with refresh: true to classify it. "+
			"The entries are grouped the way the sweep reports them -- merged, security_failures, action_required, stale, cancelled, waiting, obsolete, ci_unavailable, ci_no_verdict, skipped -- so \"what is waiting for us\" and \"what would a sweep do\" are the same question. "+
			"Rescue tooling should act on action_required only, and skip entries whose rescue object is not stale."),
		scopeArguments(),
		mcp.WithArray("teams",
			mcp.Description("Cover several teams in one call, each read under its own team file and policy. The answer is then {\"teams\": [{\"team\": ..., \"result\": ...}]}, one entry per team in the order given, and a team whose files are missing or unreadable carries an error instead of a result. "+
				"The teams share one discovery, so reading every team costs one listing rather than one per team. Mutually exclusive with team, query, org, repos and repos_file."),
			mcp.WithStringItems(),
		),
		mcp.WithBoolean("refresh",
			mcp.Description("Classify every PR again instead of reading the classification the last sweep stored in its marge/<class> label (default: false). "+
				"The stored read costs the discovery of the scope and nothing more -- one search for a query scope, or one listing per twenty-five repositories for a team scope -- and reports a PR no sweep has labelled under unclassified; a refresh costs a PR read and a check read per PR, and reports the evidence, the update type, the policy and the prior-rescue state, which the stored read leaves out."),
		),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func remedyTool() mcp.Tool {
	return mcp.NewTool("remedy",
		mcp.WithDescription("WRITES (destructive): classify one PR and apply the catalogue rule that matches it, through the action that rule names and that action's own guards. "+
			"This is the sweep engine's classify, remedy and mark steps on a single PR, so the guards, the refusals and the evidence comment are the sweep's. "+
			"A rule can never do what its action forbids, and a named rule still has to match: naming one removes the other rules from the contest, it does not force an action onto a PR. "+
			"Nothing matches means nothing is written, and the failure is reported under unhandled so it can earn a rule."),
		mcp.WithString("pr_url",
			mcp.Required(),
			mcp.Description("The pull request, as an URL (https://github.com/OWNER/REPO/pull/NUMBER) or as OWNER/REPO#NUMBER"),
		),
		mcp.WithString("rule",
			mcp.Description("Apply this rule of the catalogue instead of letting every rule compete. The rule must still match the PR."),
		),
		mcp.WithString("team",
			mcp.Description("Decide the PR under this team's policy, read from bot-prs-sweep/team-<name>.yaml in the team-file repository. Without it the company default policy applies."),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Report the rule that would apply and write nothing (default: false)"),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func markTool() mcp.Tool {
	return mcp.NewTool("mark",
		mcp.WithDescription("WRITES: record a failed AI rescue attempt on a PR by posting a machine-readable ai-rescue marker comment. Subsequent sweeps surface the marker so the operator knows a rescue was already attempted. The marker records the head SHA and a fingerprint of the PR diff: it goes stale when the PR content changes (new version, pushed fix) but survives a Renovate rebase that leaves the diff unchanged."),
		mcp.WithString("pr_url",
			mcp.Required(),
			mcp.Description("The pull request, as an URL (https://github.com/OWNER/REPO/pull/NUMBER) or as OWNER/REPO#NUMBER"),
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
		mcp.WithBoolean("dry_run",
			mcp.Description("Return the marker that would be written, head SHA and fingerprint included, and post nothing (default: false)"),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

// httpHandler routes the MCP endpoint and the probe paths. Everything else
// is a 404, so a misconfigured client gets a clear answer instead of an MCP
// error.
func httpHandler(mcpServer *server.MCPServer) (http.Handler, *server.StreamableHTTPServer) {
	streamable := server.NewStreamableHTTPServer(mcpServer,
		server.WithEndpointPath(mcpEndpoint),
		// muster attaches the person's GitHub grant as a bearer on every
		// JSON-RPC request. Carrying it on the context is what makes a
		// served call the caller's own.
		server.WithHTTPContextFunc(gh.ContextWithBearer),
	)
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	mux.Handle(mcpEndpoint, gh.RequireBearer(streamable))
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

// scopeArguments are the arguments that say what a call covers. list and
// sweep share them, so the two scopes -- a team, or a query -- mean the same
// thing on every tool and on the CLI.
func scopeArguments() mcp.ToolOption {
	options := []mcp.ToolOption{
		mcp.WithString("team",
			mcp.Description("Cover the repositories of this team, read from repositories/team-<name>.yaml in the team-file repository (giantswarm/github, or $MARGE_TEAM_FILE_REPO), under that team's policy. Mutually exclusive with query, org, repos and repos_file."),
		),
		mcp.WithString("query",
			mcp.Description("Narrow the scope the way `marge [query]` does. Without repos/repos_file the text becomes part of the GitHub search, "+
				"so it can be free text matched against the PR (a dependency name such as \"typescript\") or search qualifiers (\"repo:my-org/my-repo\"). "+
				"With repos or repos_file it keeps only the listed repositories whose org/repo contains the text (case-insensitive)."),
		),
		mcp.WithString("org",
			mcp.Description("GitHub organization or user to limit the scope to"),
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
		mcp.WithArray("prs",
			mcp.Description("Cover only these pull requests of the scope, each a PR URL or OWNER/REPO#NUMBER. "+
				"The scope still decides which repositories are read and under which policy, so a PR outside it is refused rather than acted on."),
			mcp.WithStringItems(),
		),
	}
	return func(tool *mcp.Tool) {
		for _, option := range options {
			option(tool)
		}
	}
}

// sweepTool declares the sweep tool and its arguments. parseSweepRequest
// reads exactly these arguments; the serve tests keep the two in step.
func sweepTool() mcp.Tool {
	return mcp.NewTool("sweep",
		mcp.WithDescription("Sweep bot PRs (Renovate, Align files, Herald, Dependabot): classify every open bot PR of a team or query scope, approve and squash-merge the eligible green ones, "+
			"label each with marge/<class>, and hold majors and unreadable updates for a person. A pending or unreported required check is a wait, never a bypass; a failing security check is never merged past. "+
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
		scopeArguments(),
		mcp.WithBoolean("merge_auto",
			mcp.Description("Also merge PRs that have auto-merge enabled (default: false)"),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Show what would be done without making changes (default: false). Stale PRs are still classified, but not refreshed."),
		),
		mcp.WithString("actions",
			mcp.Description("Comma-separated sweep steps to run, in fixed order: classify, approve, merge, refresh, retry, mark (default: all). refresh updates stale branches from their base; retry reruns the CircleCI workflow of auto-cancelled builds on the same head from its failed jobs, falling back to a single-build retry (needs CIRCLECI_CLI_TOKEN or ~/.circleci/cli.yml); mark writes markers and evidence comments."),
		),
		mcp.WithString("security_patterns",
			mcp.Description("Comma-separated case-insensitive substrings added to the built-in list that flags failing CI checks as security-related"),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

// sweepRequest is what one call of the sweep tool asks for: the search
// inputs and the processing options, defaulting like the sweep CLI command.
// The repos_file argument lands in Opts.ReposFile, where the CLI flag of the
// same meaning lives.
type sweepRequest struct {
	Repos []string
	// Rule narrows the catalogue to one rule before matching. Empty runs
	// the whole catalogue, which is what a sweep does.
	Rule string
	Opts RunOptions
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
	req := parseScope(request)
	req.Opts.DryRun = request.GetBool("dry_run", false)
	req.Opts.MergeAuto = request.GetBool("merge_auto", false)
	req.Opts.Actions = actions
	req.Opts.SecurityPatterns = request.GetString("security_patterns", "")
	return req, req.validate()
}

// parseScope reads the arguments scopeArguments declares. Quiet is always
// set: stdout is the MCP stdio transport, so no table, plain-text results or
// progress chatter may be written; the JSON result carries the same data.
func parseScope(request mcp.CallToolRequest) sweepRequest {
	return sweepRequest{
		Repos: request.GetStringSlice("repos", nil),
		Opts: RunOptions{
			Quiet:     true,
			NoTUI:     true,
			Query:     request.GetString("query", ""),
			Team:      request.GetString("team", ""),
			Org:       request.GetString("org", ""),
			ReposFile: request.GetString("repos_file", ""),
			PRs:       request.GetStringSlice("prs", nil),
		},
	}
}

// validate holds the same rule the sweep command enforces on its flags: the
// two scopes are exclusive, so a caller never believes a team's policy
// applied to a query it also passed.
func (r sweepRequest) validate() error {
	if r.Opts.Team != "" && (r.Opts.Query != "" || len(r.Repos) > 0 || r.Opts.Org != "" || r.Opts.ReposFile != "") {
		return errors.New("team is mutually exclusive with query, org, repos and repos_file")
	}
	return nil
}

// SweepResult is the structured JSON output returned by the sweep MCP tool.
type SweepResult struct {
	Summary SweepSummary `json:"summary"`
	// Rules says which rule catalogue the sweep ran, and what it could not
	// use. An absent catalogue leaves every remedy refused.
	Rules *SweepRules `json:"rules,omitempty"`
	// Unhandled groups the failures no rule recognised, most frequent
	// first. A signature here is what `marge rules draft` takes.
	Unhandled []SweepUnhandled `json:"unhandled,omitempty"`
	Merged    []SweepPREntry   `json:"merged,omitempty"`
	// AutoMerge lists the PRs handed to GitHub's auto-merge, which merges
	// them when their last requirement is met.
	AutoMerge []SweepPREntry `json:"auto_merge,omitempty"`
	// Remedied lists the PRs a rule of the catalogue acted on in this run:
	// a rerun, a retry, a branch update, a wait marker or a close.
	Remedied         []SweepPREntry `json:"remedied,omitempty"`
	SecurityFailures []SweepPREntry `json:"security_failures,omitempty"`
	ActionRequired   []SweepPREntry `json:"action_required,omitempty"`
	// Eligible lists green PRs the run did not merge because the merge step
	// was not among its actions. Every green PR of a list is one.
	Eligible []SweepPREntry `json:"eligible,omitempty"`
	// Unclassified lists PRs that carry no marge/<class> label, so no sweep
	// has decided them yet. Only a read of the stored classification
	// produces them: a run that classifies decides every PR it reads.
	Unclassified []SweepPREntry `json:"unclassified,omitempty"`
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
// SweepUnhandled is one shape of failure the catalogue does not recognise,
// with the PRs that carry it.
type SweepUnhandled struct {
	Signature string   `json:"signature"`
	Checks    []string `json:"checks"`
	Count     int      `json:"count"`
	PRs       []string `json:"prs"`
	// Excerpt is the log a rule would match against, from the first PR of
	// the group. Empty when no rule asked for a log.
	Excerpt string `json:"excerpt,omitempty"`
}

// SweepRules reports the rule catalogue of one sweep.
type SweepRules struct {
	// Source names the repository, ref and directory, or the local path.
	Source string `json:"source"`
	// Ref is the branch the catalogue was read from, absent for a local
	// directory.
	Ref string `json:"ref,omitempty"`
	// Digest identifies the exact documents this sweep ran, whether they
	// came from a repository or a local directory.
	Digest string `json:"digest,omitempty"`
	// Loaded counts the rules the sweep could use.
	Loaded int `json:"loaded"`
	// Skipped names the documents that failed to validate, with the reason.
	// They cost their own rule and nothing more.
	Skipped []SweepSkippedRule `json:"skipped,omitempty"`
	// Error says why no catalogue could be read at all.
	Error string `json:"error,omitempty"`
}

// SweepSkippedRule is one document the catalogue could not use.
type SweepSkippedRule struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type SweepSummary struct {
	Total  int `json:"total"`
	Merged int `json:"merged"`
	// AutoMerge counts the PRs left to GitHub's own auto-merge. They are not
	// merged: GitHub fires it when the last requirement is met.
	AutoMerge int `json:"auto_merge"`
	// Remedied counts the PRs a catalogue rule acted on.
	Remedied         int `json:"remedied"`
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
	// Eligible counts green PRs left unmerged because the merge step was
	// not among the run's actions.
	Eligible int `json:"eligible"`
	// Unclassified counts PRs no sweep has labelled. Only a read of the
	// stored classification produces them.
	Unclassified int `json:"unclassified"`
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
	// update it carries; Label the marge/<class> label that is on
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
	// RescuesDispatched says whether this build dispatches a rescue at all.
	// While it is false Timeout and Weekly are declared and neither of them
	// bounds anything.
	RescuesDispatched bool   `json:"rescues_dispatched"`
	Confirm           string `json:"confirm,omitempty"`
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
		Rescue: SweepRescuePolicy{
			Enabled:            resolved.Rescue.Enabled,
			Weekly:             resolved.Rescue.Weekly,
			BudgetPerRescueUSD: resolved.Rescue.Budget.PerRescueUSD,
			BudgetWeeklyUSD:    resolved.Rescue.Budget.WeeklyUSD,
			BudgetEnforced:     pr.BudgetEnforced,
			RescuesDispatched:  pr.RescuesDispatched,
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

// scopedPRs is what a tool starts from: the client it acts with, the login
// that client authenticates as, the PRs of the request's scope and the
// repositories the discovery could not list.
type scopedPRs struct {
	client *github.Client
	login  string
	prs    []pr.PRInfo
	found  discovery
}

// discover resolves the request's scope and finds the PRs it covers. It
// records the resolved policies on the request, so the engine decides every
// PR under the policy the scope resolved to.
func (t toolset) discover(ctx context.Context, req *sweepRequest) (scopedPRs, error) {
	client, err := t.newClient(ctx)
	if err != nil {
		return scopedPRs{}, err
	}

	scope, err := req.resolveScope(ctx, client)
	if err != nil {
		return scopedPRs{}, err
	}
	req.Opts.Policies = scope.Policies

	login, err := gh.AuthenticatedLogin(ctx, client)
	if err != nil {
		return scopedPRs{}, err
	}

	repos, err := scopeRepos(scope.Repos, req.Opts.PRs)
	if err != nil {
		return scopedPRs{}, err
	}

	found, err := searchPRs(ctx, client, req.Opts.Query, login, repos)
	if err != nil {
		return scopedPRs{}, fmt.Errorf("searching PRs: %w", err)
	}
	prs, err := filterByPRs(filterByOrg(found.PRs, req.Opts.Org), req.Opts.PRs)
	if err != nil {
		return scopedPRs{}, err
	}

	return scopedPRs{client: client, login: login, prs: prs, found: found}, nil
}

// run is the one engine call behind list, sweep and remedy. It resolves the
// scope and its policy, searches the PRs, narrows them, loads the rule
// catalogue and processes them, exactly as the sweep command does. What a
// tool changes is the request it hands in, never the path it runs.
func (t toolset) run(ctx context.Context, req sweepRequest) (SweepResult, error) {
	scoped, err := t.discover(ctx, &req)
	if err != nil {
		return SweepResult{}, err
	}
	client, login, prs, found := scoped.client, scoped.login, scoped.prs, scoped.found

	catalogue, rulesReport := loadRules(ctx, client, RulesSource{})
	if req.Rule != "" {
		catalogue, err = catalogue.Only(req.Rule)
		if err != nil {
			return SweepResult{}, err
		}
		rulesReport.Loaded = len(catalogue.Rules)
	}
	req.Opts.Rules = catalogue

	status, err := processOnceWithStatus(ctx, client, login, prs, req.Opts)
	if err != nil {
		return SweepResult{}, fmt.Errorf("processing PRs: %w", err)
	}
	return buildSweepResult(status, found.Failed, rulesReport), nil
}

// result renders a value as the tool's JSON payload.
func result(value any) (*mcp.CallToolResult, error) {
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshaling result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonBytes)), nil
}

// listRequest is what one call of the list tool asks the engine for: the
// classify step alone, on a dry run, so the call writes nothing. The stored
// read runs no action and takes the scope from it alone.
func listRequest(request mcp.CallToolRequest) (sweepRequest, error) {
	req := parseScope(request)
	req.Opts.DryRun = true
	req.Opts.NoTUI = true
	req.Opts.Actions = process.ActionSet{process.ActionClassify: true}
	return req, req.validate()
}

func (t toolset) handleList(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	refresh := request.GetBool("refresh", false)
	if teams := request.GetStringSlice("teams", nil); len(teams) > 0 {
		if err := validateTeamsScope(request); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		queues, err := t.listTeams(ctx, teams, refresh)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return result(queues)
	}

	req, err := listRequest(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	run := t.listStored
	if refresh {
		run = t.run
	}
	sweepResult, err := run(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result(sweepResult)
}

// TeamQueue is one team's queue inside a list that covered several teams.
type TeamQueue struct {
	Team   string       `json:"team"`
	Result *SweepResult `json:"result,omitempty"`
	// Error says why the team has no result: it has no team file, or its
	// files do not parse. One team's failure leaves the others alone.
	Error string `json:"error,omitempty"`
}

// TeamQueues is what the list tool answers when it was given teams. The
// answer is an object and not a bare array, so a caller can tell a
// several-team answer from a single scope's SweepResult by its shape.
type TeamQueues struct {
	Teams []TeamQueue `json:"teams"`
}

// validateTeamsScope refuses a teams list that carries a second scope, the
// way the single-team scope is refused one.
func validateTeamsScope(request mcp.CallToolRequest) error {
	if request.GetString("team", "") != "" {
		return errors.New("teams is mutually exclusive with team: pass the one team under team, or every team under teams")
	}
	if request.GetString("query", "") != "" || request.GetString("org", "") != "" ||
		request.GetString("repos_file", "") != "" || len(request.GetStringSlice("repos", nil)) > 0 {
		return errors.New("teams is mutually exclusive with query, org, repos and repos_file")
	}
	return nil
}

// teamScope is one team's resolved scope, or the error that resolving it
// produced.
type teamScope struct {
	team  string
	scope policy.Scope
	err   error
}

// listTeams reads the queues of several teams in one call.
//
// The teams share the discovery: their repository lists are merged and read
// once, so a caller that wants every team pays one listing rather than one
// per team, and a repository two teams own is read once. Everything after
// the discovery stays per team, because a team file decides what its PRs
// may become: each team's PRs are classified under that team's own policy.
func (t toolset) listTeams(ctx context.Context, teams []string, refresh bool) (TeamQueues, error) {
	client, err := t.newClient(ctx)
	if err != nil {
		return TeamQueues{}, err
	}
	loader, err := policyLoader(client)
	if err != nil {
		return TeamQueues{}, err
	}
	login, err := gh.AuthenticatedLogin(ctx, client)
	if err != nil {
		return TeamQueues{}, err
	}

	scopes := resolveTeamScopes(ctx, loader, teams)

	repoLists := make([][]string, 0, len(scopes))
	for _, scoped := range scopes {
		if scoped.err == nil {
			repoLists = append(repoLists, scoped.scope.Repos)
		}
	}
	// No team resolved, so there is nothing to read. The discovery is not
	// run at all: without repositories it would fall back to the GitHub
	// search, and answer with PRs that belong to no team asked for.
	var found discovery
	if repos := mergeRepos(repoLists...); len(repos) > 0 {
		found, err = searchPRs(ctx, client, "", login, repos)
		if err != nil {
			return TeamQueues{}, fmt.Errorf("searching PRs: %w", err)
		}
	}

	queues := make([]TeamQueue, len(scopes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, teamsAtOnce)
	for index, scoped := range scopes {
		if scoped.err != nil {
			queues[index] = TeamQueue{Team: scoped.team, Error: scoped.err.Error()}
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			queues[index] = t.teamQueue(ctx, client, login, scoped, found, refresh)
		})
	}
	wg.Wait()
	return TeamQueues{Teams: queues}, nil
}

// teamsAtOnce is how many teams of one call are classified together. It
// bounds the widest read, which is a refresh of every team: each team fans
// out over its own PRs as well.
const teamsAtOnce = 4

// teamQueue builds one team's queue from the shared discovery.
func (t toolset) teamQueue(ctx context.Context, client *github.Client, login string, scoped teamScope, found discovery, refresh bool) TeamQueue {
	prs := prsOfRepos(found.PRs, scoped.scope.Repos)
	failed := failuresOfRepos(found.Failed, scoped.scope.Repos)

	if !refresh {
		return TeamQueue{Team: scoped.team, Result: pointer(buildSweepResult(storedStatus(prs), failed, nil))}
	}

	opts := RunOptions{
		Quiet:    true,
		NoTUI:    true,
		DryRun:   true,
		Team:     scoped.team,
		Actions:  process.ActionSet{process.ActionClassify: true},
		Policies: scoped.scope.Policies,
	}
	status, err := processOnceWithStatus(ctx, client, login, prs, opts)
	if err != nil {
		return TeamQueue{Team: scoped.team, Error: err.Error()}
	}
	return TeamQueue{Team: scoped.team, Result: pointer(buildSweepResult(status, failed, nil))}
}

func pointer[T any](value T) *T { return &value }

// resolveTeamScopes reads every team's files. The reads are independent, so
// they run together: one team's files are three requests, and a caller that
// asks for every team would otherwise wait for all of them in turn.
func resolveTeamScopes(ctx context.Context, loader policy.Loader, teams []string) []teamScope {
	scopes := make([]teamScope, len(teams))
	var wg sync.WaitGroup
	slots := make(chan struct{}, scopesAtOnce)
	for index, team := range teams {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			scope, err := loader.TeamScope(ctx, team)
			scopes[index] = teamScope{team: team, scope: scope, err: err}
		})
	}
	wg.Wait()
	return scopes
}

// scopesAtOnce is how many team files are read together.
const scopesAtOnce = 8

// prsOfRepos keeps the PRs that live in one of the repositories. GitHub
// treats owner and repository names case-insensitively, so the comparison
// does too.
func prsOfRepos(prs []pr.PRInfo, repos []string) []pr.PRInfo {
	wanted := repoSet(repos)
	kept := make([]pr.PRInfo, 0, len(prs))
	for _, info := range prs {
		if wanted[strings.ToLower(info.Owner+"/"+info.Repo)] {
			kept = append(kept, info)
		}
	}
	return kept
}

// failuresOfRepos keeps the failures of one of the repositories, so a team
// is told about its own unreadable repositories and not about another
// team's.
func failuresOfRepos(failures []repoFailure, repos []string) []repoFailure {
	wanted := repoSet(repos)
	kept := make([]repoFailure, 0, len(failures))
	for _, failure := range failures {
		if wanted[strings.ToLower(failure.Repo)] {
			kept = append(kept, failure)
		}
	}
	return kept
}

func repoSet(repos []string) map[string]bool {
	set := make(map[string]bool, len(repos))
	for _, repo := range repos {
		set[strings.ToLower(strings.TrimSpace(repo))] = true
	}
	return set
}

// listStored reports the classification the last sweep stored on each PR
// instead of deciding it again. The classification lives in the PR's
// marge/<class> label, which the discovery already carries, so the whole
// list costs the discovery and nothing more: no check read, no PR read.
//
// What the label cannot say, the entry leaves out. Several states share one
// label, so the state is the class's representative; the evidence, the
// update type, the resolved policy and any prior rescue attempt each need
// the PR itself and stay empty. A PR no sweep has labelled has no stored
// classification, so it is reported as unclassified rather than guessed at.
func (t toolset) listStored(ctx context.Context, req sweepRequest) (SweepResult, error) {
	scoped, err := t.discover(ctx, &req)
	if err != nil {
		return SweepResult{}, err
	}
	return buildSweepResult(storedStatus(scoped.prs), scoped.found.Failed, nil), nil
}

// storedStatus reads each PR's stored classification out of its labels.
func storedStatus(prs []pr.PRInfo) *pr.PRStatus {
	status := pr.NewPRStatus()
	for _, info := range prs {
		idx := status.Add(info)
		status.SetClassification(idx, pr.KindOf(info.Author), "")
		label, class := pr.StoredClass(info.Labels)
		state, classified := pr.ClassState(class)
		if !classified {
			status.Update(idx, pr.StatusUnclassified, "no sweep has classified this PR")
			continue
		}
		status.Update(idx, state, "stored by the last sweep")
		status.SetLabel(idx, label)
	}
	return status
}

func (t toolset) handleSweep(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, err := parseSweepRequest(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sweepResult, err := t.run(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result(sweepResult)
}

// remedyRequest is what one call of the remedy tool asks the engine for: the
// classify, remedy and mark steps on the one PR it names. mark comes with
// remedy because the once-per-change guard reads the evidence marker.
func remedyRequest(request mcp.CallToolRequest) (sweepRequest, error) {
	prRef := request.GetString("pr_url", "")
	owner, repo, _, err := pr.ParsePRRef(prRef)
	if err != nil {
		return sweepRequest{}, err
	}
	if owner == "" || repo == "" {
		return sweepRequest{}, fmt.Errorf("not a pull request reference: %s", prRef)
	}
	return sweepRequest{
		Rule: request.GetString("rule", ""),
		Opts: RunOptions{
			Quiet:   true,
			NoTUI:   true,
			DryRun:  request.GetBool("dry_run", false),
			Team:    request.GetString("team", ""),
			PRs:     []string{prRef},
			Actions: process.ActionSet{process.ActionClassify: true, process.ActionRemedy: true, process.ActionMark: true},
		},
	}, nil
}

func (t toolset) handleRemedy(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, err := remedyRequest(request)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sweepResult, err := t.run(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result(sweepResult)
}

func buildSweepResult(status *pr.PRStatus, failed []repoFailure, sweepRules *SweepRules) SweepResult {
	counts := status.Summary()
	total := status.Len()
	securityEntries := status.SecurityFailedEntries()
	blockedEntries := status.BlockedEntries()

	result := SweepResult{
		Rules: sweepRules,
		Summary: SweepSummary{
			Total:            total,
			Merged:           counts.Merged,
			AutoMerge:        counts.AutoMerge,
			Remedied:         counts.Remedied,
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
			Eligible:         counts.Eligible,
			Unclassified:     counts.Unclassified,
		},
	}
	for _, f := range failed {
		result.RepositoriesFailed = append(result.RepositoriesFailed, SweepRepoFailure{Repo: f.Repo, Error: f.Err})
	}
	result.Unhandled = groupUnhandled(status.UnhandledEntries())

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

	for _, e := range status.AutoMergeEntries() {
		result.AutoMerge = append(result.AutoMerge, toEntry(e))
	}

	for _, e := range status.RemediedEntries() {
		result.Remedied = append(result.Remedied, toEntry(e))
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

	for _, e := range status.EligibleEntries() {
		result.Eligible = append(result.Eligible, toEntry(e))
	}

	for _, e := range status.UnclassifiedEntries() {
		result.Unclassified = append(result.Unclassified, toEntry(e))
	}

	for _, e := range status.SkippedEntries() {
		result.Skipped = append(result.Skipped, toEntry(e))
	}

	return result
}

func (t toolset) handleMark(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	prRef := request.GetString("pr_url", "")
	outcome := request.GetString("outcome", "failed")
	reason := request.GetString("reason", "")
	tool := request.GetString("tool", "ai")
	dryRun := request.GetBool("dry_run", false)

	client, err := t.newClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	marker, owner, repo, number, err := markRescue(ctx, client, prRef, outcome, reason, tool, dryRun)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	written := map[string]any{
		"owner":    owner,
		"repo":     repo,
		"number":   number,
		"outcome":  marker.Outcome,
		"tool":     marker.Tool,
		"head_sha": marker.HeadSHA,
		"at":       marker.At.Format(time.RFC3339),
		"dry_run":  dryRun,
	}
	if marker.PatchID != "" {
		written["patch_id"] = marker.PatchID
	}
	if marker.ChangeID != "" {
		written["change_id"] = marker.ChangeID
	}
	return result(written)
}

// groupUnhandled collects the unrecognised failures by signature, most
// frequent first, so a pattern worth a rule reads as a count.
func groupUnhandled(entries []pr.StatusEntry) []SweepUnhandled {
	bySignature := make(map[string]*SweepUnhandled)
	var order []string
	for _, e := range entries {
		group, seen := bySignature[e.Unhandled.Signature]
		if !seen {
			group = &SweepUnhandled{
				Signature: e.Unhandled.Signature,
				Checks:    e.Unhandled.Checks,
				Excerpt:   e.Unhandled.Excerpt,
			}
			bySignature[e.Unhandled.Signature] = group
			order = append(order, e.Unhandled.Signature)
		}
		group.Count++
		group.PRs = append(group.PRs, fmt.Sprintf("%s/%s#%d", e.PR.Owner, e.PR.Repo, e.PR.Number))
	}

	out := make([]SweepUnhandled, 0, len(order))
	for _, signature := range order {
		out = append(out, *bySignature[signature])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}
