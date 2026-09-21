package process

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

const (
	rbBase  = "1111111111111111111111111111111111111111"
	rbTitle = "chore(deps): update actions/checkout action to v5.1.0"
)

// rebaseFixture wires a fake GitHub API for two Renovate PRs of one
// repository that edit the same file: org/repo#1 merges, and org/repo#2 is
// dirty until the bot rebases it.
type rebaseFixture struct {
	// dirtyReads is how many reads of #2 report a conflict before the bot
	// rebases it. A large number means the bot never rebases.
	dirtyReads int32

	reads2  atomic.Int32
	merged1 atomic.Int32
	merged2 atomic.Int32
}

func (f *rebaseFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	pull := func(number int, head, state string) github.PullRequest {
		return github.PullRequest{
			Number:         new(number),
			Title:          new(rbTitle),
			Body:           new("| `actions/checkout` | action | minor | `v5.0.0` -> `v5.1.0` |"),
			MergeableState: new(state),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(head), Ref: new(fmt.Sprintf("renovate/branch-%d", number)), Repo: &github.Repository{FullName: new("org/repo")}},
			Base:           &github.PullRequestBranch{SHA: new(rbBase), Ref: new("main")},
		}
	}

	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, pull(1, "aaa", "clean"))
	})
	mux.HandleFunc("GET /repos/org/repo/pulls/2", func(w http.ResponseWriter, r *http.Request) {
		state := "clean"
		if f.reads2.Add(1) <= f.dirtyReads {
			state = "dirty"
		}
		writeJSON(w, pull(2, "bbb", state))
	})
	mux.HandleFunc("PUT /repos/org/repo/pulls/1/merge", func(w http.ResponseWriter, r *http.Request) {
		f.merged1.Add(1)
		writeJSON(w, github.PullRequestMergeResult{Merged: new(true)})
	})
	mux.HandleFunc("PUT /repos/org/repo/pulls/2/merge", func(w http.ResponseWriter, r *http.Request) {
		f.merged2.Add(1)
		writeJSON(w, github.PullRequestMergeResult{Merged: new(true)})
	})

	for _, head := range []string{"aaa", "bbb"} {
		mux.HandleFunc("GET /repos/org/repo/commits/"+head+"/status", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, github.CombinedStatus{State: new("success"), SHA: new(head)})
		})
	}
	for _, n := range []int{1, 2} {
		mux.HandleFunc(fmt.Sprintf("GET /repos/org/repo/commits/refs/pull/%d/head/status", n), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, github.CombinedStatus{State: new("success")})
		})
		mux.HandleFunc(fmt.Sprintf("GET /repos/org/repo/commits/refs/pull/%d/head/check-runs", n), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, github.ListCheckRunsResults{Total: new(0)})
		})
		mux.HandleFunc(fmt.Sprintf("GET /repos/org/repo/pulls/%d/reviews", n), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, []*github.PullRequestReview{})
		})
		mux.HandleFunc(fmt.Sprintf("POST /repos/org/repo/pulls/%d/reviews", n), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, github.PullRequestReview{})
		})
		mux.HandleFunc(fmt.Sprintf("GET /repos/org/repo/issues/%d/comments", n), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, []*github.IssueComment{})
		})
		mux.HandleFunc(fmt.Sprintf("POST /repos/org/repo/issues/%d/labels", n), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, []*github.Label{})
		})
	}
	for _, head := range []string{"aaa", "bbb"} {
		mux.HandleFunc("GET /repos/org/repo/compare/main..."+head, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, github.CommitsComparison{
				Status: new("ahead"), BehindBy: new(0), AheadBy: new(1),
				BaseCommit: &github.RepositoryCommit{SHA: new(rbBase)},
				Files: []*github.CommitFile{{
					Filename: new(".github/workflows/verify.yaml"),
					Patch:    new("@@ -1 +1 @@\n-      - uses: actions/checkout@v5.0.0\n+      - uses: actions/checkout@v5.1.0"),
				}},
			})
		})
	}
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.Repository{Permissions: &github.RepositoryPermissions{Push: new(true)}})
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+rbBase+"/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.CombinedStatus{State: new("success")})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+rbBase+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.ListCheckRunsResults{Total: new(0)})
	})
	// These repositories cut their release from a changelog they do not
	// keep, which is the common case and costs the sweep two reads and
	// nothing else.
	mux.HandleFunc("GET /repos/org/repo/contents/cliff.toml", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/contents/CHANGELOG.md", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sweep processes both PRs the way one repository is swept: one at a time,
// in number order.
func (f *rebaseFixture) sweep(t *testing.T, proc *Processor) (*pr.PRStatus, []int) {
	t.Helper()
	status := pr.NewPRStatus()
	var idx []int
	for _, n := range []int{1, 2} {
		info := pr.PRInfo{Owner: "org", Repo: "repo", Number: n, Title: rbTitle, Author: "renovate[bot]"}
		i := status.Add(info)
		idx = append(idx, i)
		proc.ProcessPR(t.Context(), info, status, i)
	}
	return status, idx
}

// TestRebase_SelfInflictedConflictMergesInTheSameRun is giantswarm/marge#132:
// the sweep merged #1, which conflicted #2. Nobody is needed, and #2 must
// not wait a day for the next sweep.
func TestRebase_SelfInflictedConflictMergesInTheSameRun(t *testing.T) {
	f := &rebaseFixture{dirtyReads: 1}
	proc := NewProcessor(newTestClient(t, f.server(t)), false, false, "marge[bot]")
	proc.RebaseWait = time.Second

	status, idx := f.sweep(t, proc)

	if got := status.Snapshot()[idx[1]]; got.State != pr.StatusAwaitingRebase {
		t.Fatalf("#2 after the first pass = %v (%s), want StatusAwaitingRebase", got.State, got.Detail)
	}
	if got := pr.LabelClass(pr.StatusAwaitingRebase); got != "pending" {
		t.Errorf("label class = %q, want %q: a conflict the sweep caused is not work for a person", got, "pending")
	}

	proc.Revisit(t.Context(), status)

	if got := status.Snapshot()[idx[1]]; got.State != pr.StatusMerged {
		t.Fatalf("#2 after the second pass = %v (%s), want StatusMerged", got.State, got.Detail)
	}
	if got := f.merged2.Load(); got != 1 {
		t.Errorf("#2 merged %d times, want 1", got)
	}
}

// TestRebase_NotRebasedStaysPending keeps the report honest when the bot
// does not rebase within the wait: the PR is still not a person's work, and
// the next sweep decides.
func TestRebase_NotRebasedStaysPending(t *testing.T) {
	f := &rebaseFixture{dirtyReads: 100}
	proc := NewProcessor(newTestClient(t, f.server(t)), false, false, "marge[bot]")
	proc.RebaseWait = 10 * time.Millisecond

	status, idx := f.sweep(t, proc)
	proc.Revisit(t.Context(), status)

	got := status.Snapshot()[idx[1]]
	if got.State != pr.StatusAwaitingRebase {
		t.Fatalf("#2 = %v (%s), want StatusAwaitingRebase", got.State, got.Detail)
	}
	if got := f.merged2.Load(); got != 0 {
		t.Errorf("#2 merged %d times, want 0: it is still conflicted", got)
	}
}

// TestRebase_NoMergeKeepsTheConflict holds the other side of the rule: a
// conflict the sweep did not cause is still work for a person.
func TestRebase_NoMergeKeepsTheConflict(t *testing.T) {
	f := &rebaseFixture{dirtyReads: 100}
	srv := f.server(t)
	proc := NewProcessor(newTestClient(t, srv), false, false, "marge[bot]")

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 2, Title: rbTitle, Author: "renovate[bot]"}
	idx := status.Add(info)
	proc.ProcessPR(t.Context(), info, status, idx)

	if got := status.Snapshot()[idx]; got.State != pr.StatusConflict {
		t.Fatalf("#2 = %v (%s), want StatusConflict", got.State, got.Detail)
	}
}

// TestRebase_NoOverlapCostsNothing covers the throughput criterion: a
// repository whose PRs do not conflict pays no second pass.
func TestRebase_NoOverlapCostsNothing(t *testing.T) {
	f := &rebaseFixture{}
	proc := NewProcessor(newTestClient(t, f.server(t)), false, false, "marge[bot]")
	proc.RebaseWait = time.Hour

	status, idx := f.sweep(t, proc)
	reads := f.reads2.Load()

	start := time.Now()
	proc.Revisit(t.Context(), status)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the second pass took %s with nothing to revisit", elapsed)
	}
	if got := f.reads2.Load(); got != reads {
		t.Errorf("#2 read %d more times in the second pass, want 0", got-reads)
	}
	for _, i := range idx {
		if got := status.Snapshot()[i]; got.State != pr.StatusMerged {
			t.Errorf("entry %d = %v (%s), want StatusMerged", i, got.State, got.Detail)
		}
	}
}
