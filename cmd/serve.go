package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(serveCmd)
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start a stdio MCP server exposing sweep as a tool",
	Long: `Start a Model Context Protocol (MCP) server over stdio.
The server exposes a "sweep" tool that mirrors the sweep CLI command,
returning structured JSON results instead of terminal output.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mcpServer := server.NewMCPServer(
			"marge",
			version,
			server.WithToolCapabilities(true),
		)

		mcpServer.AddTool(
			mcp.NewTool("sweep",
				mcp.WithDescription("Sweep dependency update PRs: find, approve, and merge Renovate/Dependabot PRs. "+
					"Returns structured JSON: summary counts plus merged, security_failures, action_required, stale, refreshed, cancelled, retried, ci_unavailable and skipped lists. "+
					"A failing PR whose head is behind its base branch and whose every failing check is green on the base branch head is classified as stale "+
					"(the failure was fixed on the base branch after the PR's last build) and listed under stale, not action_required; "+
					"set refresh_stale to update such branches from their base so CI re-runs (they are then listed under refreshed). "+
					"A failing PR whose every failing check is a CircleCI build that CircleCI itself auto-cancelled (a newer pipeline on the branch, a redundant workflow) "+
					"is classified as cancelled and listed under cancelled, not action_required: there is no verdict on the code yet; "+
					"set retry_cancelled to retry such builds on the same commit (they are then listed under retried). "+
					"Rescue tooling should act on action_required only, and skip entries whose rescue object is not stale: "+
					"a prior automated rescue already failed on exactly this change (rebased: true means the branch was merely rebased since, the attempt still stands)."),
				mcp.WithString("org",
					mcp.Description("GitHub organization or user to limit the sweep to"),
				),
				mcp.WithString("repos_file",
					mcp.Description("Path to a file listing org/repo entries (one per line) to scan for bot PRs"),
				),
				mcp.WithArray("repos",
					mcp.Description("Explicit list of repos (org/repo format) to sweep"),
					mcp.WithStringItems(),
				),
				mcp.WithBoolean("merge_auto",
					mcp.Description("Also merge PRs that have auto-merge enabled (default: false)"),
				),
				mcp.WithBoolean("dry_run",
					mcp.Description("Show what would be done without making changes (default: false). Stale PRs are still classified, but not refreshed."),
				),
				mcp.WithBoolean("refresh_stale",
					mcp.Description("Update the branch of stale PRs from their base (same as GitHub's \"Update branch\" button) so CI re-runs, and report them under refreshed (default: false). Skipped for PRs carrying a non-stale ai-rescue marker."),
				),
				mcp.WithBoolean("retry_cancelled",
					mcp.Description("Retry CircleCI builds that CircleCI auto-cancelled on the PR's current head so the same commit gets a real verdict, and report them under retried (default: false). Needs a CircleCI token (CIRCLECI_CLI_TOKEN or ~/.circleci/cli.yml)."),
				),
				mcp.WithString("author",
					mcp.Description("Filter by PR author: \"renovate\", \"dependabot\", or \"all\" (default: \"all\")"),
					mcp.Enum("renovate", "dependabot", "all"),
				),
				mcp.WithString("trusted_authors",
					mcp.Description("Comma-separated list of trusted PR author logins (default: \"renovate[bot],dependabot[bot]\")"),
				),
				mcp.WithString("security_patterns",
					mcp.Description("Comma-separated list of case-insensitive substrings used to flag failing CI checks as security-related (defaults to a built-in list)"),
				),
			),
			handleSweep,
		)

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

		return server.ServeStdio(mcpServer)
	},
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
	// branch refresh (refresh_stale), not a rescue, so they are excluded from
	// action_required.
	Stale []SweepPREntry `json:"stale,omitempty"`
	// Refreshed lists stale PRs whose branch was updated from its base in
	// this run. CI is running again; the next sweep decides what they are.
	Refreshed []SweepPREntry `json:"refreshed,omitempty"`
	// Cancelled lists failing PRs whose every failing check is a CircleCI
	// build that CircleCI itself auto-cancelled: there is no verdict on the
	// code yet. The remedy is a retry (retry_cancelled), not a rescue, so
	// they are excluded from action_required. A build cancelled behind a
	// newer head is listed too but never retried: the new head's own build
	// is the verdict.
	Cancelled []SweepPREntry `json:"cancelled,omitempty"`
	// Retried lists cancelled PRs whose builds were retried on the same
	// commit in this run. CI is running again; the next sweep decides.
	Retried []SweepPREntry `json:"retried,omitempty"`
	// CIUnavailable lists PRs whose CI could not run because a GitHub Actions
	// budget / spending-limit block prevented every job from starting. These
	// are NOT failures: the remedy is to raise or await the Actions budget,
	// so they are reported separately and excluded from action_required.
	CIUnavailable []SweepPREntry `json:"ci_unavailable,omitempty"`
	Skipped       []SweepPREntry `json:"skipped,omitempty"`
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
	Skipped   int `json:"skipped"`
}

// SweepPREntry represents a single PR in the sweep results.
type SweepPREntry struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	AgeDays   int    `json:"age_days,omitempty"`
	// Rescue describes the most recent prior automated rescue attempt
	// found on the PR (an ai-rescue marker comment), if any. Consumers
	// dispatching rescue agents should skip entries with a non-stale
	// failed rescue and escalate them to a human instead.
	Rescue *SweepRescueInfo `json:"rescue,omitempty"`
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
	// Extract parameters from the request.
	org := request.GetString("org", "")
	reposFile := request.GetString("repos_file", "")
	mergeAuto := request.GetBool("merge_auto", false)
	dryRun := request.GetBool("dry_run", false)
	refreshStale := request.GetBool("refresh_stale", false)
	retryCancelled := request.GetBool("retry_cancelled", false)
	author := request.GetString("author", "all")
	trustedAuthors := request.GetString("trusted_authors", "renovate[bot],dependabot[bot]")
	securityPatterns := request.GetString("security_patterns", "")
	reposParam := request.GetStringSlice("repos", nil)

	// Create a temporary repos file if repos array was provided.
	if len(reposParam) > 0 && reposFile == "" {
		tmpFile, err := createTempReposFile(reposParam)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("creating temp repos file: %v", err)), nil
		}
		reposFile = tmpFile
	}

	client, err := gh.NewClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("creating GitHub client: %v", err)), nil
	}

	me, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("getting authenticated user: %v", err)), nil
	}
	login := me.GetLogin()

	prs, err := searchPRs(ctx, client, "", login, author, reposFile)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("searching PRs: %v", err)), nil
	}

	if org != "" {
		filtered := prs[:0]
		for _, p := range prs {
			if strings.EqualFold(p.Owner, org) {
				filtered = append(filtered, p)
			}
		}
		prs = filtered
	}

	// Quiet: stdout is the MCP stdio transport, so no table, plain-text
	// results or progress chatter may be written; the JSON result below
	// carries the same data.
	opts := RunOptions{
		DryRun:           dryRun,
		MergeAuto:        mergeAuto,
		RefreshStale:     refreshStale,
		RetryCancelled:   retryCancelled,
		Quiet:            true,
		Author:           author,
		TrustedAuthors:   trustedAuthors,
		SecurityPatterns: securityPatterns,
	}

	status, err := processOnceWithStatus(ctx, client, login, prs, opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("processing PRs: %v", err)), nil
	}

	result := buildSweepResult(status)

	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshaling results: %v", err)), nil
	}

	return mcp.NewToolResultText(string(jsonBytes)), nil
}

func buildSweepResult(status *pr.PRStatus) SweepResult {
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
			Stale:            counts.Stale,
			Refreshed:        counts.Refreshed,
			Cancelled:        counts.Cancelled,
			Retried:          counts.Retried,
			Skipped:          counts.Skipped,
		},
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
		}
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

func createTempReposFile(repos []string) (string, error) {
	f, err := os.CreateTemp("", "marge-repos-*.txt")
	if err != nil {
		return "", err
	}

	for _, repo := range repos {
		if _, err := fmt.Fprintln(f, repo); err != nil {
			_ = f.Close()
			return "", err
		}
	}

	if err := f.Close(); err != nil {
		return "", err
	}

	return f.Name(), nil
}
