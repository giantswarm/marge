package process

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// commentOnlyPatch is the diff of giantswarm/agentgateway#29: the pinned
// commit is unchanged and only the trailing version comment moved.
const commentOnlyPatch = `@@ -12,7 +12,7 @@ jobs:
     steps:
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0
       - name: Build
         run: make build`

// fxSourceVersion is the version the fixture's Renovate PR updates from.
const fxSourceVersion = "v5.0.0"

const realPatch = `@@ -12,7 +12,7 @@ jobs:
     steps:
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8 # v7.0.1
       - name: Build
         run: make build`

// siblingFixture wires a fake GitHub API for one Renovate PR (org/repo#29)
// whose state and diff the knobs choose.
type siblingFixture struct {
	title      string
	mergeable  string
	failing    bool
	patch      string
	supersedes pr.SupersededBy
	extraFiles int

	compareCalls atomic.Int32
}

func (f *siblingFixture) run(t *testing.T) pr.StatusEntry {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mergeable := f.mergeable
	if mergeable == "" {
		mergeable = "clean"
	}
	body := fmt.Sprintf("| `actions/checkout` | action | minor | `%s` -> `%s` |",
		fxSourceVersion, pr.ExtractTargetVersion(f.title))
	mux.HandleFunc("GET /repos/org/repo/pulls/29", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.PullRequest{
			Number:         new(29),
			Title:          new(f.title),
			Body:           new(body),
			MergeableState: new(mergeable),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(fxHead), Ref: new("renovate/actions-checkout-5.x"), Repo: &github.Repository{FullName: new("org/repo")}},
			Base:           &github.PullRequestBranch{SHA: new(fxBase), Ref: new("main")},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/29/head/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.CombinedStatus{State: new("success"), SHA: new(fxHead)})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/29/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		res := github.ListCheckRunsResults{Total: new(0)}
		if f.failing {
			res.Total = new(1)
			res.CheckRuns = []*github.CheckRun{{
				ID: new(int64(1)), Name: new("lint"),
				Status: new("completed"), Conclusion: new("failure"),
			}}
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("GET /repos/org/repo/compare/main..."+fxHead, func(w http.ResponseWriter, r *http.Request) {
		f.compareCalls.Add(1)
		files := []*github.CommitFile{{
			Filename: new(".github/workflows/ci.yaml"),
			Patch:    new(f.patch),
		}}
		for i := range f.extraFiles {
			files = append(files, &github.CommitFile{
				Filename: new(fmt.Sprintf(".github/workflows/gen-%d.yaml", i)),
				Patch:    new(f.patch),
			})
		}
		writeJSON(w, github.CommitsComparison{
			Status: new("ahead"), BehindBy: new(0), AheadBy: new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(fxBase)},
			Files:      files,
		})
	})
	// The dry-run path checks the reviewer's write access before it reports
	// a skip, so a green PR reaches this handler.
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.Repository{Permissions: &github.RepositoryPermissions{Push: new(true)}})
	})
	mux.HandleFunc("GET /repos/org/repo/issues/29/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []*github.IssueComment{})
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_pull_request_reviews", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.CombinedStatus{State: new("success")})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.ListCheckRunsResults{Total: new(0)})
	})
	// A dry run reads this too: it says what a real run would write, and
	// only the repository knows whether an entry applies to it.
	mux.HandleFunc("GET /repos/org/repo/contents/cliff.toml", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	proc := NewProcessor(newTestClient(t, srv), true, false, "me")
	proc.SupersededBy = f.supersedes
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 29, Title: f.title, Author: "renovate[bot]"}
	idx := status.Add(info)
	proc.ProcessPR(t.Context(), info, status, idx)
	return status.Snapshot()[idx]
}

func TestSuperseded_reportedInsteadOfAConflict(t *testing.T) {
	// giantswarm/agentgateway#29: reported as a merge conflict although #30
	// carries the same bump at a higher version. Rescuing it is work that
	// does not exist.
	f := &siblingFixture{
		title:     "chore(deps): update actions/checkout action to v5.1.0",
		mergeable: "dirty",
		patch:     commentOnlyPatch,
		supersedes: pr.SupersededBy{
			"org/repo#29": "superseded by #30 (v7.0.1)",
		},
	}
	got := f.run(t)

	if got.State != pr.StatusObsolete {
		t.Fatalf("state = %v (%s), want StatusObsolete", got.State, got.Detail)
	}
	if got.ObsoleteReason != pr.ReasonSuperseded {
		t.Errorf("reason = %q, want %q", got.ObsoleteReason, pr.ReasonSuperseded)
	}
	if got.Detail != "superseded by #30 (v7.0.1)" {
		t.Errorf("detail = %q", got.Detail)
	}
	if got.Rescue != nil {
		t.Error("an obsolete PR must not carry a rescue marker: closing it is the remedy")
	}
	if f.compareCalls.Load() != 0 {
		t.Errorf("compare called %d times, want 0: supersession is decided from the PR list alone", f.compareCalls.Load())
	}
}

func TestSuperseded_greenPRStillMerges(t *testing.T) {
	// A supersession never suppresses a merge: the sibling can be a major
	// that CI rejects, and the safe lower bump must still land.
	f := &siblingFixture{
		title: "chore(deps): update actions/checkout action to v5.1.0",
		patch: commentOnlyPatch,
		supersedes: pr.SupersededBy{
			"org/repo#29": "superseded by #30 (v7.0.1)",
		},
	}
	got := f.run(t)

	if got.State != pr.StatusEligible {
		t.Fatalf("state = %v (%s), want StatusEligible in dry run: a green PR takes the merge path", got.State, got.Detail)
	}
}

func TestNoOp_reportedInsteadOfAConflict(t *testing.T) {
	f := &siblingFixture{
		title:     "chore(deps): update actions/checkout action to v5.1.0",
		mergeable: "dirty",
		patch:     commentOnlyPatch,
	}
	got := f.run(t)

	if got.State != pr.StatusObsolete || got.ObsoleteReason != pr.ReasonNoOp {
		t.Fatalf("state = %v/%q (%s), want StatusObsolete/no_op", got.State, got.ObsoleteReason, got.Detail)
	}
	if got.Detail != noOpDetail {
		t.Errorf("detail = %q", got.Detail)
	}
	if got.Rescue != nil {
		t.Error("a no-op PR must not carry a rescue marker: there is nothing to fix")
	}
}

func TestNoOp_reportedInsteadOfAFailure(t *testing.T) {
	f := &siblingFixture{
		title:   "chore(deps): update actions/checkout action to v5.1.0",
		failing: true,
		patch:   commentOnlyPatch,
	}
	got := f.run(t)

	if got.State != pr.StatusObsolete || got.ObsoleteReason != pr.ReasonNoOp {
		t.Fatalf("state = %v/%q (%s), want StatusObsolete/no_op", got.State, got.ObsoleteReason, got.Detail)
	}
}

func TestNoOp_aTruncatedFileListIsNoEvidence(t *testing.T) {
	// The compare API caps its file list at 300 without saying so. A verdict
	// read off a partial list would be a guess, so the PR stays a failure.
	f := &siblingFixture{
		title:      "chore(deps): update actions/checkout action to v5.1.0",
		failing:    true,
		patch:      commentOnlyPatch,
		extraFiles: compareFileLimit,
	}
	got := f.run(t)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed", got.State, got.Detail)
	}
}

func TestNoOp_realConflictStaysAConflict(t *testing.T) {
	f := &siblingFixture{
		title:     "chore(deps): update actions/checkout action to v7.0.1",
		mergeable: "dirty",
		patch:     realPatch,
	}
	got := f.run(t)

	if got.State != pr.StatusConflict {
		t.Fatalf("state = %v (%s), want StatusConflict: the pinned SHA really moved", got.State, got.Detail)
	}
}

func TestNoOp_realFailureStaysAFailure(t *testing.T) {
	f := &siblingFixture{
		title:   "chore(deps): update actions/checkout action to v7.0.1",
		failing: true,
		patch:   realPatch,
	}
	got := f.run(t)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed", got.State, got.Detail)
	}
	if f.compareCalls.Load() != 1 {
		t.Errorf("compare called %d times on the failure path, want 1: staleness and the no-op rule share it",
			f.compareCalls.Load())
	}
}

func TestNoOp_greenPRPaysNoDiffRequest(t *testing.T) {
	f := &siblingFixture{title: "chore(deps): update actions/checkout action to v5.1.0", patch: commentOnlyPatch}
	got := f.run(t)

	if got.State != pr.StatusEligible {
		t.Fatalf("state = %v (%s), want StatusEligible in dry run", got.State, got.Detail)
	}
	if f.compareCalls.Load() != 0 {
		t.Errorf("compare called %d times for a green PR, want 0", f.compareCalls.Load())
	}
}

func TestObsolete_keptOutOfTheFailedCounts(t *testing.T) {
	status := pr.NewPRStatus()
	sup := status.Add(pr.PRInfo{Owner: "org", Repo: "repo", Number: 29})
	status.MarkObsolete(sup, pr.ReasonSuperseded, "superseded by #30 (v7.0.1)")
	noop := status.Add(pr.PRInfo{Owner: "org", Repo: "repo", Number: 31})
	status.MarkObsolete(noop, pr.ReasonNoOp, noOpDetail)

	counts := status.Summary()
	if counts.Failed != 0 {
		t.Errorf("failed count = %d, want 0", counts.Failed)
	}
	if counts.Obsolete != 2 {
		t.Errorf("obsolete = %d, want 2", counts.Obsolete)
	}
	if len(status.ActionRequired()) != 0 {
		t.Error("an obsolete PR does not belong in the action-required list")
	}
	entries := status.ObsoleteEntries()
	if len(entries) != 2 {
		t.Fatalf("obsolete entries = %d, want 2", len(entries))
	}
	if entries[0].ObsoleteReason != pr.ReasonSuperseded || entries[1].ObsoleteReason != pr.ReasonNoOp {
		t.Errorf("reasons = %q, %q", entries[0].ObsoleteReason, entries[1].ObsoleteReason)
	}
}
