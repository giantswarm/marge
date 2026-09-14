package process

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/google/go-github/v91/github"
)

// staleFixture wires a fake GitHub API for one Renovate PR (org/repo#1,
// head "aaa111", base "main") whose only failing check is a check run named
// "go-build". Knobs choose how far the branch is behind main, how go-build
// looks on the main head ("bbb222"), which marker comments the PR carries,
// and whether the failing check is a check run or a commit status.
type staleFixture struct {
	behindBy int
	// baseConclusion is go-build's conclusion on the base head; "" means
	// the check does not exist there at all.
	baseConclusion string
	// failAsStatus makes the PR's failing check a commit status context
	// instead of a check run.
	failAsStatus bool
	// baseAsStatus reports go-build on the base head as a commit status.
	baseAsStatus bool
	comments     []string
	// title, changedFiles and files describe the PR's own change: the
	// compare response lists files (with patches) and the PR reports how
	// many it changed. Rescue-marker fingerprints are computed from them.
	title        string
	changedFiles int
	files        []*github.CommitFile

	updateBranchCalls atomic.Int32
	compareCalls      atomic.Int32
	baseLookupCalls   atomic.Int32
}

const (
	fxHead = "aaa111aaa111aaa111aaa111aaa111aaa111aaa1"
	fxBase = "bbb222bbb222bbb222bbb222bbb222bbb222bbb2"
)

var fxGreenSince = time.Date(2026, 9, 5, 10, 57, 0, 0, time.UTC)

func (f *staleFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.PullRequest{
			Number:         new(1),
			Title:          new(f.title),
			ChangedFiles:   new(f.changedFiles),
			MergeableState: new("behind"),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(fxHead), Ref: new("renovate/foo")},
			Base:           &github.PullRequestBranch{SHA: new(fxBase), Ref: new("main")},
		})
	})

	// PR head: go-build failed, either as a check run or a commit status.
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, r *http.Request) {
		cs := github.CombinedStatus{State: new("success")}
		if f.failAsStatus {
			cs.State = new("failure")
			cs.Statuses = []*github.RepoStatus{{Context: new("go-build"), State: new("failure")}}
		}
		writeJSON(w, cs)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		res := github.ListCheckRunsResults{Total: new(0)}
		if !f.failAsStatus {
			res.Total = new(1)
			res.CheckRuns = []*github.CheckRun{{
				ID: new(int64(11)), Name: new("go-build"),
				Status: new("completed"), Conclusion: new("failure"),
			}}
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("GET /repos/org/repo/compare/main..."+fxHead, func(w http.ResponseWriter, r *http.Request) {
		f.compareCalls.Add(1)
		status := "ahead"
		if f.behindBy > 0 {
			status = "behind"
		}
		writeJSON(w, github.CommitsComparison{
			Status:     new(status),
			BehindBy:   new(f.behindBy),
			AheadBy:    new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(fxBase)},
			Files:      f.files,
		})
	})

	// Base head: go-build as configured, plus an unrelated always-green
	// check so the lookups never come back empty.
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/status", func(w http.ResponseWriter, r *http.Request) {
		f.baseLookupCalls.Add(1)
		cs := github.CombinedStatus{State: new("success"), Statuses: []*github.RepoStatus{
			{Context: new("lint"), State: new("success"), UpdatedAt: &github.Timestamp{Time: fxGreenSince.Add(-time.Hour)}},
		}}
		if f.baseAsStatus && f.baseConclusion != "" {
			cs.Statuses = append(cs.Statuses, &github.RepoStatus{
				Context: new("go-build"), State: new(f.baseConclusion),
				UpdatedAt: &github.Timestamp{Time: fxGreenSince},
			})
		}
		writeJSON(w, cs)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
		res := github.ListCheckRunsResults{Total: new(0)}
		if !f.baseAsStatus && f.baseConclusion != "" {
			res.Total = new(1)
			res.CheckRuns = []*github.CheckRun{{
				ID: new(int64(21)), Name: new("go-build"),
				Status: new("completed"), Conclusion: new(f.baseConclusion),
				CompletedAt: &github.Timestamp{Time: fxGreenSince},
			}}
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		var out []*github.IssueComment
		for i, body := range f.comments {
			out = append(out, &github.IssueComment{ID: new(int64(i + 1)), Body: new(body)})
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("PUT /repos/org/repo/pulls/1/update-branch", func(w http.ResponseWriter, r *http.Request) {
		f.updateBranchCalls.Add(1)
		// GitHub answers 202 Accepted: the update is scheduled.
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, github.PullRequestBranchUpdateResponse{Message: new("Updating pull request branch.")})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	return httptest.NewServer(mux)
}

// runStale processes org/repo#1 through ProcessPR and returns its final
// status entry.
func (f *staleFixture) run(t *testing.T, configure func(*Processor)) pr.StatusEntry {
	t.Helper()
	server := f.server(t)
	defer server.Close()

	proc := NewProcessor(newTestClient(t, server), false, false, "me", DefaultTrustedAuthors)
	if configure != nil {
		configure(proc)
	}
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1, Author: "renovate[bot]"}
	idx := status.Add(info)

	proc.ProcessPR(context.Background(), info, status, idx)
	return status.Snapshot()[idx]
}

// markerComment renders a legacy marker comment: head SHA only, no
// fingerprint, as written before patch_id/change_id existed.
func markerComment(headSHA string) string {
	return markerCommentWith(headSHA, pr.Fingerprint{})
}

// markerCommentWith renders a marker comment pinning headSHA and fp.
func markerCommentWith(headSHA string, fp pr.Fingerprint) string {
	extra := ""
	if fp.PatchID != "" {
		extra += fmt.Sprintf(",\"patch_id\":%q", fp.PatchID)
	}
	if fp.ChangeID != "" {
		extra += fmt.Sprintf(",\"change_id\":%q", fp.ChangeID)
	}
	return fmt.Sprintf("**AI rescue failed** (klaus): nope\n\n<!-- ai-rescue: {\"tool\":\"klaus\",\"outcome\":\"failed\",\"reason\":\"nope\",\"head_sha\":%q%s,\"at\":\"2026-09-04T18:40:00Z\"} -->", headSHA, extra)
}

func TestClassifyStale_behindAndGreenOnBase(t *testing.T) {
	f := &staleFixture{behindBy: 12, baseConclusion: "success"}
	got := f.run(t, nil)

	if got.State != pr.StatusStale {
		t.Fatalf("state = %v (%s), want StatusStale", got.State, got.Detail)
	}
	for _, want := range []string{"go-build", "green on main", "since 2026-09-05 10:57 UTC", "12 behind"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q should contain %q", got.Detail, want)
		}
	}
	if f.updateBranchCalls.Load() != 0 {
		t.Errorf("update-branch called %d times without --refresh-stale, want 0", f.updateBranchCalls.Load())
	}
}

func TestClassifyStale_behindAndGreenAsCommitStatus(t *testing.T) {
	// CircleCI-style: the failing context and its green twin on main are
	// commit statuses, not check runs.
	f := &staleFixture{behindBy: 3, baseConclusion: "success", failAsStatus: true, baseAsStatus: true}
	got := f.run(t, nil)

	if got.State != pr.StatusStale {
		t.Fatalf("state = %v (%s), want StatusStale", got.State, got.Detail)
	}
}

func TestClassifyStale_behindButRedOnBase(t *testing.T) {
	f := &staleFixture{behindBy: 5, baseConclusion: "failure"}
	got := f.run(t, nil)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed: the failure is real on main too", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "go-build") {
		t.Errorf("detail %q should name the failing check", got.Detail)
	}
}

func TestClassifyStale_behindButAbsentOnBase(t *testing.T) {
	// A PR-only check cannot be proven green on main: stay conservative.
	f := &staleFixture{behindBy: 5, baseConclusion: ""}
	got := f.run(t, nil)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed", got.State, got.Detail)
	}
}

func TestClassifyStale_notBehind(t *testing.T) {
	f := &staleFixture{behindBy: 0, baseConclusion: "success"}
	got := f.run(t, nil)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed: head already includes main", got.State, got.Detail)
	}
	if f.compareCalls.Load() != 1 {
		t.Errorf("compare called %d times, want 1", f.compareCalls.Load())
	}
	if f.baseLookupCalls.Load() != 0 {
		t.Errorf("base status looked up %d times for a non-behind PR, want 0", f.baseLookupCalls.Load())
	}
}

func TestClassifyStale_refreshUpdatesBranch(t *testing.T) {
	f := &staleFixture{behindBy: 12, baseConclusion: "success"}
	got := f.run(t, func(p *Processor) { p.RefreshStale = true })

	if got.State != pr.StatusRefreshed {
		t.Fatalf("state = %v (%s), want StatusRefreshed", got.State, got.Detail)
	}
	if !strings.HasPrefix(got.Detail, "re-checking; ") || !strings.Contains(got.Detail, "go-build green on main") {
		t.Errorf("detail = %q, want re-checking plus the stale reason", got.Detail)
	}
	if f.updateBranchCalls.Load() != 1 {
		t.Errorf("update-branch called %d times, want 1", f.updateBranchCalls.Load())
	}
}

func TestClassifyStale_dryRunOnlyClassifies(t *testing.T) {
	f := &staleFixture{behindBy: 12, baseConclusion: "success"}
	got := f.run(t, func(p *Processor) { p.RefreshStale = true; p.DryRun = true })

	if got.State != pr.StatusStale {
		t.Fatalf("state = %v (%s), want StatusStale in dry run", got.State, got.Detail)
	}
	if f.updateBranchCalls.Load() != 0 {
		t.Errorf("update-branch called %d times in dry run, want 0", f.updateBranchCalls.Load())
	}
}

func TestClassifyStale_refreshSkippedForFreshMarker(t *testing.T) {
	f := &staleFixture{behindBy: 12, baseConclusion: "success", comments: []string{markerComment(fxHead[:8])}}
	got := f.run(t, func(p *Processor) { p.RefreshStale = true })

	if got.State != pr.StatusStale {
		t.Fatalf("state = %v (%s), want StatusStale (refresh skipped)", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "refresh skipped") {
		t.Errorf("detail %q should say the refresh was skipped", got.Detail)
	}
	if f.updateBranchCalls.Load() != 0 {
		t.Errorf("update-branch called %d times despite a fresh marker, want 0", f.updateBranchCalls.Load())
	}
	if got.Rescue == nil || got.Rescue.Stale {
		t.Fatalf("rescue = %+v, want a non-stale marker attached", got.Rescue)
	}
}

func TestClassifyStale_refreshProceedsPastStaleMarker(t *testing.T) {
	f := &staleFixture{behindBy: 12, baseConclusion: "success", comments: []string{markerComment("0ld5ha00")}}
	got := f.run(t, func(p *Processor) { p.RefreshStale = true })

	if got.State != pr.StatusRefreshed {
		t.Fatalf("state = %v (%s), want StatusRefreshed", got.State, got.Detail)
	}
	if f.updateBranchCalls.Load() != 1 {
		t.Errorf("update-branch called %d times, want 1", f.updateBranchCalls.Load())
	}
	if got.Rescue == nil || !got.Rescue.Stale {
		t.Fatalf("rescue = %+v, want the stale marker attached for the operator", got.Rescue)
	}
}

func TestClassifyStale_markerAttachedWithoutRefresh(t *testing.T) {
	// Without --refresh-stale the deferred marker lookup still annotates
	// stale entries, like it does for failures.
	f := &staleFixture{behindBy: 12, baseConclusion: "success", comments: []string{markerComment(fxHead)}}
	got := f.run(t, nil)

	if got.State != pr.StatusStale {
		t.Fatalf("state = %v (%s), want StatusStale", got.State, got.Detail)
	}
	if got.Rescue == nil {
		t.Fatal("rescue marker should be attached to a stale entry")
	}
}

func TestBaseContextStates_cachedPerBaseCommit(t *testing.T) {
	f := &staleFixture{behindBy: 4, baseConclusion: "success"}
	server := f.server(t)
	defer server.Close()

	proc := NewProcessor(newTestClient(t, server), false, false, "me", DefaultTrustedAuthors)
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	for i := 0; i < 3; i++ {
		states, err := proc.baseContextStates(context.Background(), info, fxBase)
		if err != nil {
			t.Fatalf("baseContextStates: %v", err)
		}
		if st := states["go-build"]; !st.green() || !st.at.Equal(fxGreenSince) {
			t.Fatalf("go-build state = %+v, want green since %s", st, fxGreenSince)
		}
	}
	if f.baseLookupCalls.Load() != 1 {
		t.Errorf("base status fetched %d times for the same commit, want 1 (cached)", f.baseLookupCalls.Load())
	}
}

func TestContextState_green(t *testing.T) {
	tests := []struct {
		name string
		st   contextState
		want bool
	}{
		{"success only", contextState{success: true}, true},
		{"failure only", contextState{failed: true}, false},
		{"success and failure under one name", contextState{success: true, failed: true}, false},
		{"pending", contextState{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.st.green(); got != tt.want {
				t.Errorf("green() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStaleResult_detail(t *testing.T) {
	r := &staleResult{Base: "main", BehindBy: 7, Contexts: []string{"go-build", "lint"}, GreenSince: fxGreenSince}
	want := "go-build, lint green on main since 2026-09-05 10:57 UTC, 7 behind"
	if got := r.detail(); got != want {
		t.Errorf("detail() = %q, want %q", got, want)
	}
	bare := &staleResult{Base: "main", Contexts: []string{"go-build"}}
	if got := bare.detail(); got != "go-build green on main" {
		t.Errorf("detail() without time/behind = %q", got)
	}
}
