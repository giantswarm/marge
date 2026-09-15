package process

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// msgSetupWorkflow is the message recorded on oncall-shift-reporter#44,
// 2026-07-11.
const msgSetupWorkflow = "Use of setup workflows must be enabled in project settings " +
	"(Project settings > Advanced -> Dynamic config using setup workflows)"

const msgBudgetBlock = "The job was not started because an Actions budget is preventing further use."

func TestIsSetupWorkflowBlock(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{"circleci message", msgSetupWorkflow, true},
		// A commit-status description is capped at 140 characters, so the
		// message can arrive cut short.
		{"truncated description", "Use of setup workflows must be enabled in project settings (Project sett", true},
		{"case-insensitive", "SETUP WORKFLOWS MUST BE ENABLED", true},
		{"real failure", "Your tests failed on CircleCI", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSetupWorkflowBlock(tt.msg); got != tt.want {
				t.Errorf("isSetupWorkflowBlock(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestMatchesAnyMessage_scansEveryField(t *testing.T) {
	if !matchesAnyMessage(isSetupWorkflowBlock, "", msgSetupWorkflow, "", nil) {
		t.Error("a match in the summary must be found")
	}
	if !matchesAnyMessage(isSetupWorkflowBlock, "", "", "", []string{"unrelated", msgSetupWorkflow}) {
		t.Error("a match in an annotation must be found")
	}
	if matchesAnyMessage(isSetupWorkflowBlock, "Build failed", "1 test failed", "want 200, got 500", []string{"assertion failed"}) {
		t.Error("a genuine failure must not match")
	}
}

func TestClassifyCheckRunMessages(t *testing.T) {
	tests := []struct {
		name       string
		summary    string
		annotation string
		wantKind   checkKind
		wantReason string
	}{
		{"budget block", msgBudgetBlock, "", kindBudgetBlock, ""},
		{"setup workflow", msgSetupWorkflow, "", kindNoVerdict, circleCISetupReason},
		{"setup workflow in annotation", "", msgSetupWorkflow, kindNoVerdict, circleCISetupReason},
		{"genuine failure", "2 tests failed", "want 200, got 500", kindRealFailure, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var annotations []string
			if tt.annotation != "" {
				annotations = []string{tt.annotation}
			}
			kind, reason := classifyCheckRunMessages("", tt.summary, "", annotations)
			if kind != tt.wantKind || reason != tt.wantReason {
				t.Errorf("classifyCheckRunMessages = (%q, %q), want (%q, %q)", kind, reason, tt.wantKind, tt.wantReason)
			}
		})
	}
}

func TestNoVerdictDetail(t *testing.T) {
	tests := []struct {
		name   string
		checks []noVerdictCheck
		want   string
	}{
		{"none", nil, "checks produced no verdict"},
		{
			"one",
			[]noVerdictCheck{{Name: "publish / gitleaks", Reason: cancelledReason}},
			"checks produced no verdict: publish / gitleaks (cancelled before it finished)",
		},
		{
			"two",
			[]noVerdictCheck{
				{Name: "publish / gitleaks", Reason: cancelledReason},
				{Name: "pre-commit", Reason: cancelledReason},
			},
			"checks produced no verdict: publish / gitleaks (cancelled before it finished), " +
				"pre-commit (cancelled before it finished)",
		},
		{
			"more than three are summarized",
			[]noVerdictCheck{
				{Name: "a", Reason: "r"},
				{Name: "b", Reason: "r"},
				{Name: "c", Reason: "r"},
				{Name: "d", Reason: "r"},
			},
			"checks produced no verdict: a (r), b (r), c (r) (+1 more)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := noVerdictDetail(tt.checks); got != tt.want {
				t.Errorf("noVerdictDetail = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetupWorkflowReason_namesTheSetting(t *testing.T) {
	want := "CircleCI setup workflows disabled; " +
		"enable Project settings > Advanced > Dynamic config using setup workflows"
	if circleCISetupReason != want {
		t.Errorf("circleCISetupReason = %q, want %q", circleCISetupReason, want)
	}
}

func TestDedupeByName(t *testing.T) {
	in := []noVerdictCheck{
		{Name: "CircleCI Pipeline", Reason: circleCISetupReason},
		{Name: "gitleaks", Reason: cancelledReason},
		{Name: "CircleCI Pipeline", Reason: circleCISetupReason},
	}
	got := dedupeByName(in)
	if len(got) != 2 || got[0].Name != "CircleCI Pipeline" || got[1].Name != "gitleaks" {
		t.Errorf("dedupeByName = %v, want the first entry per name in order", got)
	}
}

// noVerdictFixture wires a fake GitHub API for one Renovate PR (org/repo#1)
// whose checks report failure. Knobs choose the check runs and the commit
// statuses so each classification can be driven end to end through ProcessPR.
type noVerdictFixture struct {
	checkRuns   []*github.CheckRun
	statuses    []*github.RepoStatus
	annotations map[int64][]string
	// combinedState overrides the combined commit-status state, which the
	// fixture otherwise derives from statuses.
	combinedState string
}

func (f *noVerdictFixture) run(t *testing.T) pr.StatusEntry {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.PullRequest{
			Number:         new(1),
			Title:          new("Update module github.com/giantswarm/klausctl to v0.2.40"),
			MergeableState: new("clean"),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(fxHead), Ref: new("renovate/klausctl")},
			Base:           &github.PullRequestBranch{SHA: new(fxBase), Ref: new("main")},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, r *http.Request) {
		state := f.combinedState
		if state == "" {
			state = "success"
			if len(f.statuses) > 0 {
				state = "failure"
			}
		}
		writeJSON(w, github.CombinedStatus{State: new(state), SHA: new(fxHead), Statuses: f.statuses})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.ListCheckRunsResults{Total: new(len(f.checkRuns)), CheckRuns: f.checkRuns})
	})
	mux.HandleFunc("GET /repos/org/repo/check-runs/{id}/annotations", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		_, _ = fmt.Sscanf(r.PathValue("id"), "%d", &id)
		out := make([]*github.CheckRunAnnotation, 0, len(f.annotations[id]))
		for _, m := range f.annotations[id] {
			out = append(out, &github.CheckRunAnnotation{Message: new(m), AnnotationLevel: new("failure")})
		}
		writeJSON(w, out)
	})
	// The branch is ahead of main, so nothing is ever classified as stale.
	mux.HandleFunc("GET /repos/org/repo/compare/main..."+fxHead, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.CommitsComparison{
			Status: new("ahead"), BehindBy: new(0), AheadBy: new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(fxBase)},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []*github.IssueComment{})
	})
	// No required checks, and nothing reported on the base head: a red
	// check on the PR is never pre-existing here.
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.CombinedStatus{State: new("success"), SHA: new(fxBase)})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.ListCheckRunsResults{Total: new(0)})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	proc := NewProcessor(newTestClient(t, srv), true, false, "me")
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1, Author: "renovate[bot]"}
	idx := status.Add(info)
	proc.ProcessPR(context.Background(), info, status, idx)
	return status.Snapshot()[idx]
}

func failedRun(id int64, name, conclusion string) *github.CheckRun {
	return &github.CheckRun{
		ID: new(id), Name: new(name),
		Status: new("completed"), Conclusion: new(conclusion),
	}
}

func TestNoVerdict_cancelledSecurityScanIsNotASecurityFailure(t *testing.T) {
	// kserve#62: the gitleaks job was cancelled after 2h57m. Nothing was
	// scanned, so this is not a finding.
	f := &noVerdictFixture{checkRuns: []*github.CheckRun{failedRun(1, "publish / gitleaks", "cancelled")}}
	got := f.run(t)

	if got.State != pr.StatusNoVerdict {
		t.Fatalf("state = %v (%s), want StatusNoVerdict", got.State, got.Detail)
	}
	if got.Detail != "checks produced no verdict: publish / gitleaks (cancelled before it finished)" {
		t.Errorf("detail = %q", got.Detail)
	}
}

func TestNoVerdict_timedOutJobStaysAFailure(t *testing.T) {
	// A timed-out job usually ran and hung: a dependency bump that
	// deadlocks a test produces exactly this shape. Reporting it as "rerun
	// the checks" hides a real regression behind a rerun that hangs again.
	f := &noVerdictFixture{checkRuns: []*github.CheckRun{failedRun(1, "govulncheck", "timed_out")}}
	got := f.run(t)

	if got.State != pr.StatusFailedSecurity {
		t.Fatalf("state = %v (%s), want StatusFailedSecurity: a timeout is not proof the job never ran", got.State, got.Detail)
	}
}

func TestNoVerdict_realSecurityFindingStaysASecurityFailure(t *testing.T) {
	run := failedRun(1, "publish / gitleaks", "failure")
	run.Output = &github.CheckRunOutput{
		Title:   new("1 secret detected"),
		Summary: new("gitleaks found a hardcoded AWS key in config/dev.yaml"),
	}
	f := &noVerdictFixture{checkRuns: []*github.CheckRun{run}}
	got := f.run(t)

	if got.State != pr.StatusFailedSecurity {
		t.Fatalf("state = %v (%s), want StatusFailedSecurity: the scan ran and found a secret", got.State, got.Detail)
	}
}

func TestNoVerdict_realFailureAlongsideACancelledJobStaysFailed(t *testing.T) {
	f := &noVerdictFixture{checkRuns: []*github.CheckRun{
		failedRun(1, "pre-commit", "cancelled"),
		failedRun(2, "go-test", "failure"),
	}}
	got := f.run(t)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed: go-test really failed", got.State, got.Detail)
	}
	if got.Detail != "checks failed: go-test" {
		t.Errorf("detail = %q, want the cancelled job left out of the failure list", got.Detail)
	}
}

func TestNoVerdict_setupWorkflowStatusNamesTheSetting(t *testing.T) {
	// oncall-shift-reporter#44: CircleCI refuses the pipeline until the
	// project setting is on. No branch fix and no rerun resolves it.
	f := &noVerdictFixture{statuses: []*github.RepoStatus{{
		Context:     new("ci/circleci: CircleCI Pipeline"),
		State:       new("failure"),
		Description: new(msgSetupWorkflow),
	}}}
	got := f.run(t)

	if got.State != pr.StatusNoVerdict {
		t.Fatalf("state = %v (%s), want StatusNoVerdict", got.State, got.Detail)
	}
	want := "checks produced no verdict: ci/circleci: CircleCI Pipeline (" + circleCISetupReason + ")"
	if got.Detail != want {
		t.Errorf("detail = %q\nwant     %q", got.Detail, want)
	}
	if got.Rescue != nil {
		t.Error("a project-setting block must not carry a rescue marker: no code change fixes it")
	}
}

func TestNoVerdict_setupWorkflowCheckRun(t *testing.T) {
	run := failedRun(1, "CircleCI Pipeline", "failure")
	run.Output = &github.CheckRunOutput{Summary: new(msgSetupWorkflow)}
	f := &noVerdictFixture{checkRuns: []*github.CheckRun{run}}
	got := f.run(t)

	if got.State != pr.StatusNoVerdict {
		t.Fatalf("state = %v (%s), want StatusNoVerdict", got.State, got.Detail)
	}
}

func TestNoVerdict_sameCheckFromStatusAndCheckRunIsListedOnce(t *testing.T) {
	run := failedRun(1, "ci/circleci: CircleCI Pipeline", "failure")
	run.Output = &github.CheckRunOutput{Summary: new(msgSetupWorkflow)}
	f := &noVerdictFixture{
		checkRuns: []*github.CheckRun{run},
		statuses: []*github.RepoStatus{{
			Context:     new("ci/circleci: CircleCI Pipeline"),
			State:       new("failure"),
			Description: new(msgSetupWorkflow),
		}},
	}
	got := f.run(t)

	want := "checks produced no verdict: ci/circleci: CircleCI Pipeline (" + circleCISetupReason + ")"
	if got.Detail != want {
		t.Errorf("detail = %q\nwant     %q", got.Detail, want)
	}
}

func TestNoVerdict_realFailureAlongsideASettingBlockStaysFailed(t *testing.T) {
	f := &noVerdictFixture{
		checkRuns: []*github.CheckRun{failedRun(1, "go-test", "failure")},
		statuses: []*github.RepoStatus{{
			Context:     new("ci/circleci: CircleCI Pipeline"),
			State:       new("failure"),
			Description: new(msgSetupWorkflow),
		}},
	}
	got := f.run(t)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed: go-test really failed", got.State, got.Detail)
	}
}

func TestNoVerdict_budgetBlockKeepsItsOwnBucket(t *testing.T) {
	// The combined state is "failure" while no individual commit status
	// failed, so the budget bucket must win over the bare combined state.
	run := failedRun(1, "publish / gitleaks", "failure")
	run.Output = &github.CheckRunOutput{AnnotationsCount: new(1)}
	f := &noVerdictFixture{
		combinedState: "failure",
		checkRuns:     []*github.CheckRun{run},
		annotations:   map[int64][]string{1: {msgBudgetBlock}},
	}
	got := f.run(t)

	if got.State != pr.StatusBlockedCI {
		t.Fatalf("state = %v (%s), want StatusBlockedCI", got.State, got.Detail)
	}
}

func TestNoVerdict_keptOutOfTheFailedCounts(t *testing.T) {
	status := pr.NewPRStatus()
	idx := status.Add(pr.PRInfo{Owner: "org", Repo: "repo", Number: 1})
	status.Update(idx, pr.StatusNoVerdict, "checks produced no verdict: gitleaks (cancelled before it finished)")

	counts := status.Summary()
	if counts.Failed != 0 {
		t.Errorf("failed count = %d, want 0", counts.Failed)
	}
	if counts.NoVerdict != 1 {
		t.Errorf("no-verdict count = %d, want 1", counts.NoVerdict)
	}
	if len(status.ActionRequired()) != 0 {
		t.Error("the bucket does not belong in the action-required list")
	}
	if len(status.NoVerdictEntries()) != 1 {
		t.Error("the bucket must list its entry")
	}
}
