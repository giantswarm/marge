package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
)

// TestBuildSweepResult_failedAndSecurityAreDisjoint guards the contract
// documented on SweepSummary: a security failure must be counted in
// SecurityFailures and not in Failed, so consumers can sum them without
// double-counting.
func TestBuildSweepResult_failedAndSecurityAreDisjoint(t *testing.T) {
	status := pr.NewPRStatus()
	idx1 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx1, pr.StatusFailed, "checks failed")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusFailedSecurity, "security check failed: trivy")
	idx3 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 3})
	status.Update(idx3, pr.StatusMerged, "squash")
	idx4 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 4})
	status.Update(idx4, pr.StatusSkipped, "dry-run")

	got := buildSweepResult(status, nil, nil)

	if got.Summary.Total != 4 {
		t.Errorf("Total = %d, want 4", got.Summary.Total)
	}
	if got.Summary.Merged != 1 {
		t.Errorf("Merged = %d, want 1", got.Summary.Merged)
	}
	if got.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (security failures must be excluded)", got.Summary.Failed)
	}
	if got.Summary.SecurityFailures != 1 {
		t.Errorf("SecurityFailures = %d, want 1", got.Summary.SecurityFailures)
	}
	if got.Summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", got.Summary.Skipped)
	}

	if len(got.SecurityFailures) != 1 {
		t.Fatalf("SecurityFailures slice len = %d, want 1", len(got.SecurityFailures))
	}
	if got.SecurityFailures[0].Number != 2 {
		t.Errorf("SecurityFailures[0].Number = %d, want 2", got.SecurityFailures[0].Number)
	}

	if len(got.ActionRequired) != 1 {
		t.Fatalf("ActionRequired slice len = %d, want 1", len(got.ActionRequired))
	}
	if got.ActionRequired[0].Number != 1 {
		t.Errorf("ActionRequired[0].Number = %d, want 1", got.ActionRequired[0].Number)
	}
}

// TestBuildSweepResult_ciUnavailableIsSeparate guards that a PR blocked by a
// GitHub Actions budget is reported under ci_unavailable and excluded from
// both the failed count and action_required, so rescue tooling never picks
// it up.
func TestBuildSweepResult_ciUnavailableIsSeparate(t *testing.T) {
	status := pr.NewPRStatus()
	idx1 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx1, pr.StatusFailed, "checks failed: build")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusBlockedCI, "Actions budget exhausted; no jobs ran: Test, Lint")

	got := buildSweepResult(status, nil, nil)

	if got.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (budget block must be excluded)", got.Summary.Failed)
	}
	if got.Summary.CIUnavailable != 1 {
		t.Errorf("CIUnavailable = %d, want 1", got.Summary.CIUnavailable)
	}
	if len(got.CIUnavailable) != 1 {
		t.Fatalf("CIUnavailable slice len = %d, want 1", len(got.CIUnavailable))
	}
	if got.CIUnavailable[0].Number != 2 {
		t.Errorf("CIUnavailable[0].Number = %d, want 2", got.CIUnavailable[0].Number)
	}
	for _, e := range got.ActionRequired {
		if e.Number == 2 {
			t.Error("budget-blocked PR must not appear in action_required")
		}
	}
}

// TestBuildSweepResult_staleAndRefreshedAreSeparate guards that a stale
// failure (fixed on the base branch already) and a refreshed branch are
// reported under their own keys and excluded from failed/action_required,
// so rescue tooling never dispatches an agent for a branch that only
// needs a refresh.
func TestBuildSweepResult_staleAndRefreshedAreSeparate(t *testing.T) {
	status := pr.NewPRStatus()
	idx1 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx1, pr.StatusFailed, "checks failed: build")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusStale, "go-build green on main since 2026-09-05 10:57 UTC, 5 behind")
	idx3 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 3})
	status.Update(idx3, pr.StatusRefreshed, "re-checking; go-build green on main since 2026-09-05 10:57 UTC, 5 behind")

	got := buildSweepResult(status, nil, nil)

	if got.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (stale/refreshed must be excluded)", got.Summary.Failed)
	}
	if got.Summary.Stale != 1 || got.Summary.Refreshed != 1 {
		t.Errorf("Stale = %d, Refreshed = %d, want 1 and 1", got.Summary.Stale, got.Summary.Refreshed)
	}
	if len(got.Stale) != 1 || got.Stale[0].Number != 2 || got.Stale[0].Status != "Stale" {
		t.Errorf("Stale = %+v, want only #2 with status Stale", got.Stale)
	}
	if len(got.Refreshed) != 1 || got.Refreshed[0].Number != 3 || got.Refreshed[0].Status != "Refreshed" {
		t.Errorf("Refreshed = %+v, want only #3 with status Refreshed", got.Refreshed)
	}
	if len(got.ActionRequired) != 1 || got.ActionRequired[0].Number != 1 {
		t.Errorf("ActionRequired = %+v, want only #1", got.ActionRequired)
	}
}

// TestBuildSweepResult_cancelledAndRetriedAreSeparate guards that a PR whose
// CircleCI build was auto-cancelled (no verdict yet) and one whose build was
// just retried are reported under their own keys and excluded from
// failed/action_required, so rescue tooling never dispatches an agent for a
// build that only needs a retry.
func TestBuildSweepResult_cancelledAndRetriedAreSeparate(t *testing.T) {
	status := pr.NewPRStatus()
	idx1 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx1, pr.StatusFailed, "checks failed: ci/circleci: go-build")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusCancelled, "build 1263 auto-cancelled; retry needed")
	idx3 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 3})
	status.Update(idx3, pr.StatusRetried, "re-checking; build 1263 retried as 1272")

	got := buildSweepResult(status, nil, nil)

	if got.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (cancelled/retried must be excluded)", got.Summary.Failed)
	}
	if got.Summary.Cancelled != 1 || got.Summary.Retried != 1 {
		t.Errorf("Cancelled = %d, Retried = %d, want 1 and 1", got.Summary.Cancelled, got.Summary.Retried)
	}
	if len(got.Cancelled) != 1 || got.Cancelled[0].Number != 2 || got.Cancelled[0].Status != "Cancelled" {
		t.Errorf("Cancelled = %+v, want only #2 with status Cancelled", got.Cancelled)
	}
	if len(got.Retried) != 1 || got.Retried[0].Number != 3 || got.Retried[0].Status != "Retried" {
		t.Errorf("Retried = %+v, want only #3 with status Retried", got.Retried)
	}
	if len(got.ActionRequired) != 1 || got.ActionRequired[0].Number != 1 {
		t.Errorf("ActionRequired = %+v, want only #1", got.ActionRequired)
	}
}

// TestBuildSweepResult_rescueRebased guards that the rescue object tells a
// marker whose branch was merely rebased (still valid) apart from a stale
// one, so orchestrators keep skipping the rebased PR.
func TestBuildSweepResult_rescueRebased(t *testing.T) {
	status := pr.NewPRStatus()
	idx := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx, pr.StatusFailed, "checks failed: build")
	status.SetRescue(idx, &pr.RescueMarker{Tool: "klaus", Outcome: "blocked", Reason: "peer dep", Rebased: true})

	got := buildSweepResult(status, nil, nil)

	if len(got.ActionRequired) != 1 || got.ActionRequired[0].Rescue == nil {
		t.Fatalf("ActionRequired = %+v, want one entry with a rescue object", got.ActionRequired)
	}
	rescue := got.ActionRequired[0].Rescue
	if rescue.Stale || !rescue.Rebased {
		t.Errorf("rescue = %+v, want stale=false rebased=true", rescue)
	}
}

// sweepArguments sets every argument the sweep tool declares to a value
// that differs from its default, so a parsed request shows whether each one
// was read.
var sweepArguments = map[string]any{
	"query":             "typescript",
	"org":               "my-org",
	"repos_file":        "/tmp/repos.txt",
	"repos":             []any{"my-org/a", "my-org/b"},
	"merge_auto":        true,
	"dry_run":           true,
	"team":              "",
	"actions":           "merge",
	"security_patterns": "Trivy,Analyze",
}

// TestParseSweepRequest_readsEveryDeclaredArgument guards the tool schema
// against its parser: an argument declared but never read would be
// silently ignored by the server, and one read but never declared would be
// invisible to clients. Both directions are checked through sweepArguments.
func TestParseSweepRequest_readsEveryDeclaredArgument(t *testing.T) {
	declared := sweepTool().InputSchema.Properties
	for name := range declared {
		if _, ok := sweepArguments[name]; !ok {
			t.Errorf("sweep tool declares %q but sweepArguments does not set it", name)
		}
	}
	for name := range sweepArguments {
		if _, ok := declared[name]; !ok {
			t.Errorf("sweepArguments sets %q but the sweep tool does not declare it", name)
		}
	}

	got, err := parseSweepRequest(mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: sweepArguments}})
	if err != nil {
		t.Fatalf("parseSweepRequest: %v", err)
	}
	want := sweepRequest{
		Query: "typescript",
		Repos: []string{"my-org/a", "my-org/b"},
		Opts: RunOptions{
			DryRun:           true,
			MergeAuto:        true,
			Quiet:            true,
			Actions:          process.ActionSet{process.ActionClassify: true, process.ActionMerge: true},
			Org:              "my-org",
			ReposFile:        "/tmp/repos.txt",
			SecurityPatterns: "Trivy,Analyze",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSweepRequest = %+v, want %+v", got, want)
	}
}

// TestParseSweepRequest_teamScope guards the two scopes: team resolves the
// repositories; team together with any query-scope argument is refused.
func TestParseSweepRequest_teamScope(t *testing.T) {
	got, err := parseSweepRequest(mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"team": "bumblebee"}}})
	if err != nil {
		t.Fatalf("parseSweepRequest: %v", err)
	}
	if got.Opts.Team != "bumblebee" || got.Opts.CheckTimeout != 0 {
		t.Errorf("team request = %+v, want team bumblebee with zero check timeout", got.Opts)
	}

	for _, extra := range []map[string]any{{"query": "x"}, {"org": "o"}, {"repos": []any{"o/r"}}, {"repos_file": "/tmp/f"}} {
		args := map[string]any{"team": "bumblebee"}
		for k, v := range extra {
			args[k] = v
		}
		if _, err := parseSweepRequest(mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}); err == nil {
			t.Errorf("team with %v: want an error", extra)
		}
	}

	if _, err := parseSweepRequest(mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{"actions": "rescue"}}}); err == nil {
		t.Error("unknown action: want an error")
	}
}

// TestParseSweepRequest_defaultsMatchSweepCommand guards that a call with no
// arguments behaves like a bare `marge sweep --query ""`: every action, no
// wait for checks, no query, and Quiet because stdout is the transport.
func TestParseSweepRequest_defaultsMatchSweepCommand(t *testing.T) {
	got, err := parseSweepRequest(mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("parseSweepRequest: %v", err)
	}
	allActions, _ := process.ParseActions("")
	want := sweepRequest{
		Opts: RunOptions{
			Quiet:   true,
			Actions: allActions,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSweepRequest(empty) = %+v, want %+v", got, want)
	}
}

// TestMergeRepos guards the repos plus repos_file merge: entries are
// trimmed, duplicates (GitHub names are case-insensitive) collapse onto
// their first spelling, order is kept, and no entry at all yields nil so
// the GitHub search still runs.
func TestMergeRepos(t *testing.T) {
	tests := []struct {
		name  string
		lists [][]string
		want  []string
	}{
		{"no lists is nil", nil, nil},
		{"empty lists are nil", [][]string{nil, {}}, nil},
		{"blank entries are dropped", [][]string{{"", "  "}}, nil},
		{"one list is kept in order", [][]string{{"o/b", "o/a"}}, []string{"o/b", "o/a"}},
		{"lists are concatenated", [][]string{{"o/a"}, {"o/b"}}, []string{"o/a", "o/b"}},
		{"entries are trimmed", [][]string{{" o/a "}}, []string{"o/a"}},
		{"duplicates keep the first spelling", [][]string{{"o/a", "O/A"}, {"o/A", "o/b"}}, []string{"o/a", "o/b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeRepos(tt.lists...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mergeRepos(%v) = %v, want %v", tt.lists, got, tt.want)
			}
		})
	}
}

// TestSweepRequest_resolveScope guards that the sweep tool honours repos
// and repos_file together instead of dropping one: the merged list holds
// the explicit entries first, then the file's entries, without duplicates
// and without ever touching a temporary file.
func TestSweepRequest_resolveScope(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", nil)

	reposFile := filepath.Join(t.TempDir(), "repos.txt")
	content := "# team repos\n\nmy-org/a\n  My-Org/c  \nother/d\n"
	if err := os.WriteFile(reposFile, []byte(content), 0o600); err != nil {
		t.Fatalf("writing repos file: %v", err)
	}

	tests := []struct {
		name string
		req  sweepRequest
		want []string
	}{
		{"neither is nil", sweepRequest{}, nil},
		{"repos only", sweepRequest{Repos: []string{"my-org/b", "my-org/a"}}, []string{"my-org/b", "my-org/a"}},
		{"repos_file only", sweepRequest{Opts: RunOptions{ReposFile: reposFile}}, []string{"my-org/a", "My-Org/c", "other/d"}},
		{
			"both are merged without duplicates",
			sweepRequest{Repos: []string{"my-org/b", "my-org/c"}, Opts: RunOptions{ReposFile: reposFile}},
			[]string{"my-org/b", "my-org/c", "my-org/a", "other/d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, err := tt.req.resolveScope(t.Context(), client)
			if err != nil {
				t.Fatalf("resolveScope: %v", err)
			}
			if !reflect.DeepEqual(scope.Repos, tt.want) {
				t.Errorf("resolveScope = %v, want %v", scope.Repos, tt.want)
			}
		})
	}

	t.Run("unreadable repos_file is an error", func(t *testing.T) {
		req := sweepRequest{Repos: []string{"my-org/b"}, Opts: RunOptions{ReposFile: filepath.Join(t.TempDir(), "missing.txt")}}
		scope, err := req.resolveScope(t.Context(), client)
		if err == nil || !strings.Contains(err.Error(), "reading repos file") {
			t.Fatalf("resolveScope = %v, %v; want a reading repos file error", scope.Repos, err)
		}
	})
}
