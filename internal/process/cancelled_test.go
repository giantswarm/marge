package process

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
)

// The CircleCI fixtures are the recorded v1.1 builds from giantswarm/mcp-capi
// that motivated the classification (2026-09-09/10):
//
//	build 883  a real failure (govulncheck step failed) on d698f9ca
//	build 1244 auto-cancelled on 2c0ce64c; Renovate then pushed 605a2d69
//	build 1263 auto-cancelled on 7090e923, the PR's current head
const (
	cxHeadCancelled = "7090e9230aeea78db24aa97c7903ef2d393f2c36"
	cxHeadMoved     = "605a2d695c1abb382eba2d623edf2b9fe496179b"

	fixtureFailed          = "build-883-failed.json"
	fixtureCancelledBehind = "build-1244-auto-cancelled.json"
	fixtureCancelledHead   = "build-1263-auto-cancelled.json"
)

// circleStatus is one failing "ci/circleci: <job>" commit status on the PR
// head and the recorded build the fake CircleCI API serves behind it.
type circleStatus struct {
	context string
	num     int
	fixture string
	// workflowID replaces the workflow id recorded in the fixture. An
	// explicit "" (see noWorkflow) models a build that runs outside a
	// workflow, which only the v1.1 single-build retry can rerun.
	workflowID string
	noWorkflow bool
}

// fxWorkflowID is the workflow id recorded in every cancelled fixture: all
// of them come from the same mcp-capi "build" workflow.
const fxWorkflowID = "ea42abad-ba5a-4169-854b-001d55b79c1a"

// cancelledFixture wires a fake GitHub API for one Renovate PR (org/repo#1,
// base main) whose failing checks are CircleCI commit statuses, plus a fake
// CircleCI v1.1 API serving recorded builds. Knobs choose the PR head, the
// failing statuses, an additional failing GitHub Actions check run, whether
// the CircleCI project is private (404 without a token) and how far the
// branch is behind main with the failing job green there.
type cancelledFixture struct {
	head            string
	statuses        []circleStatus
	checkRunFailure bool
	private         bool
	token           string
	behindBy        int
	// rerunStatus overrides the HTTP status the fake v2 rerun endpoint
	// answers with. Zero means 202 Accepted.
	rerunStatus int

	buildCalls      atomic.Int32
	retryCalls      atomic.Int32
	rerunCalls      atomic.Int32
	baseLookupCalls atomic.Int32

	mu           sync.Mutex
	circleTokens []string
	retryPaths   []string
	rerunPaths   []string
	rerunBodies  []string
}

func (f *cancelledFixture) github(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.PullRequest{
			Number:         new(1),
			Title:          new("Update module github.com/giantswarm/mcp-oauth to v1.3.20"),
			MergeableState: new("clean"),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(f.head), Ref: new("renovate/mcp-oauth")},
			Base:           &github.PullRequestBranch{SHA: new(fxBase), Ref: new("main")},
		})
	})

	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, r *http.Request) {
		cs := github.CombinedStatus{State: new("failure"), SHA: new(f.head)}
		for _, s := range f.statuses {
			cs.Statuses = append(cs.Statuses, &github.RepoStatus{
				Context:     new(s.context),
				State:       new("failure"),
				Description: new("Your tests failed on CircleCI"),
				TargetURL:   new(fmt.Sprintf("https://circleci.com/gh/giantswarm/mcp-capi/%d", s.num)),
			})
		}
		writeJSON(w, cs)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		res := github.ListCheckRunsResults{Total: new(0)}
		if f.checkRunFailure {
			res.Total = new(1)
			res.CheckRuns = []*github.CheckRun{{
				ID: new(int64(11)), Name: new("lint"),
				Status: new("completed"), Conclusion: new("failure"),
			}}
		}
		writeJSON(w, res)
	})

	// Stale classification: behind as configured, with every failing
	// context green on the base head.
	mux.HandleFunc("GET /repos/org/repo/compare/main..."+f.head, func(w http.ResponseWriter, r *http.Request) {
		status := "ahead"
		if f.behindBy > 0 {
			status = "behind"
		}
		writeJSON(w, github.CommitsComparison{
			Status:     new(status),
			BehindBy:   new(f.behindBy),
			AheadBy:    new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(fxBase)},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/status", func(w http.ResponseWriter, r *http.Request) {
		f.baseLookupCalls.Add(1)
		cs := github.CombinedStatus{State: new("success")}
		for _, s := range f.statuses {
			cs.Statuses = append(cs.Statuses, &github.RepoStatus{
				Context: new(s.context), State: new("success"),
				UpdatedAt: &github.Timestamp{Time: fxGreenSince},
			})
		}
		writeJSON(w, cs)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+fxBase+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.ListCheckRunsResults{Total: new(0)})
	})

	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []*github.IssueComment{})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux)
}

func (f *cancelledFixture) circle(t *testing.T) *httptest.Server {
	t.Helper()
	byNum := make(map[int]circleStatus, len(f.statuses))
	for _, s := range f.statuses {
		byNum[s.num] = s
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Circle-Token")
		f.mu.Lock()
		f.circleTokens = append(f.circleTokens, token)
		f.mu.Unlock()
		if f.private && token == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Build not found"}`))
			return
		}

		var num int
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v2/workflow/") {
			if token == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Permission denied"}`))
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.rerunCalls.Add(1)
			f.mu.Lock()
			f.rerunPaths = append(f.rerunPaths, r.URL.Path)
			f.rerunBodies = append(f.rerunBodies, string(body))
			f.mu.Unlock()
			if f.rerunStatus != 0 {
				w.WriteHeader(f.rerunStatus)
				_, _ = w.Write([]byte(`{"message":"Permission denied"}`))
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"workflow_id":"9b9c4a0e-0000-4000-8000-000000000001"}`))
			return
		}

		if r.Method == http.MethodPost {
			if _, err := fmt.Sscanf(r.URL.Path, "/api/v1.1/project/github/giantswarm/mcp-capi/%d/retry", &num); err != nil {
				t.Errorf("unexpected CircleCI request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			if token == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Permission denied"}`))
				return
			}
			f.retryCalls.Add(1)
			f.mu.Lock()
			f.retryPaths = append(f.retryPaths, r.URL.Path)
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"build_num": %d, "status": "queued", "lifecycle": "queued", "vcs_revision": %q, "retry_of": %d}`, num+9, f.head, num)
			return
		}

		if _, err := fmt.Sscanf(r.URL.Path, "/api/v1.1/project/github/giantswarm/mcp-capi/%d", &num); err != nil || byNum[num].fixture == "" {
			t.Errorf("unexpected CircleCI request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		f.buildCalls.Add(1)
		data, err := os.ReadFile(filepath.Join("..", "circleci", "testdata", byNum[num].fixture))
		if err != nil {
			t.Fatalf("reading fixture: %v", err)
		}
		_, _ = w.Write(retargetWorkflow(data, byNum[num]))
	})
	return httptest.NewServer(mux)
}

// run processes org/repo#1 through ProcessPR against both fakes and returns
// its final status entry. A nil configure leaves the processor at its
// defaults; the CircleCI client is attached unless configure sets it to nil.
func (f *cancelledFixture) run(t *testing.T, configure func(*Processor)) pr.StatusEntry {
	t.Helper()
	gh := f.github(t)
	defer gh.Close()
	cc := f.circle(t)
	defer cc.Close()

	proc := NewProcessor(newTestClient(t, gh), false, false, "me", DefaultTrustedAuthors)
	proc.CircleCI = &circleci.Client{HTTPClient: cc.Client(), BaseURL: cc.URL, Token: f.token}
	if configure != nil {
		configure(proc)
	}
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1, Author: "renovate[bot]"}
	idx := status.Add(info)

	proc.ProcessPR(context.Background(), info, status, idx)
	return status.Snapshot()[idx]
}

// retargetWorkflow rewrites the workflow id of a recorded build so one
// fixture can stand in for several workflows, or for a build that runs
// outside a workflow at all.
func retargetWorkflow(data []byte, s circleStatus) []byte {
	switch {
	case s.noWorkflow:
		return bytes.ReplaceAll(data, []byte(`"`+fxWorkflowID+`"`), []byte(`""`))
	case s.workflowID != "":
		return bytes.ReplaceAll(data, []byte(fxWorkflowID), []byte(s.workflowID))
	}
	return data
}

func goBuildStatus(num int, fixture string) []circleStatus {
	return []circleStatus{{context: "ci/circleci: go-build", num: num, fixture: fixture}}
}

func TestClassifyCancelled_realFailureStaysFailed(t *testing.T) {
	f := &cancelledFixture{head: "d698f9ca512a0abdceceb441f582c0faa8fc6722", statuses: goBuildStatus(883, fixtureFailed)}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true; p.CircleCI.Token = "tok" })

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed: govulncheck really failed", got.State, got.Detail)
	}
	if got.Detail != "checks failed: ci/circleci: go-build" {
		t.Errorf("detail = %q, want the plain failure without a note", got.Detail)
	}
	if f.buildCalls.Load() != 1 || f.retryCalls.Load() != 0 {
		t.Errorf("build lookups = %d, retries = %d; want 1 and 0", f.buildCalls.Load(), f.retryCalls.Load())
	}
}

func TestClassifyCancelled_behindNewerHead(t *testing.T) {
	// Build 1244 was cancelled on 2c0ce64c; the PR head is 605a2d69 now.
	f := &cancelledFixture{head: cxHeadMoved, statuses: goBuildStatus(1244, fixtureCancelledBehind), token: "tok"}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled", got.State, got.Detail)
	}
	want := "build 1244 (2c0ce64) auto-cancelled; head is now 605a2d6, its build is the verdict"
	if got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
	if f.retryCalls.Load() != 0 {
		t.Errorf("retry called %d times for a build behind the head, want 0: the new head's build decides", f.retryCalls.Load())
	}
}

func TestClassifyCancelled_onHead(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead)}
	got := f.run(t, nil)

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled", got.State, got.Detail)
	}
	if got.Detail != "build 1263 auto-cancelled; retry needed" {
		t.Errorf("detail = %q", got.Detail)
	}
	if f.retryCalls.Load() != 0 {
		t.Errorf("retry called %d times without --retry-cancelled, want 0", f.retryCalls.Load())
	}
	if got.Rescue != nil {
		t.Errorf("rescue = %+v, want none: a cancelled build is not a code failure", got.Rescue)
	}
}

func TestClassifyCancelled_retryOnHead(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead), token: "tok"}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusRetried {
		t.Fatalf("state = %v (%s), want StatusRetried", got.State, got.Detail)
	}
	if got.Detail != "re-checking; workflow build rerun from failed" {
		t.Errorf("detail = %q", got.Detail)
	}
	// The workflow rerun, not the single-build retry: only the former
	// releases the downstream jobs the cancel left blocked.
	if f.rerunCalls.Load() != 1 || f.rerunPaths[0] != "/api/v2/workflow/"+fxWorkflowID+"/rerun" {
		t.Errorf("rerun calls = %d %q, want one POST to the v2 workflow rerun endpoint", f.rerunCalls.Load(), f.rerunPaths)
	}
	if f.retryCalls.Load() != 0 {
		t.Errorf("v1.1 build retry called %d times for a build inside a workflow, want 0", f.retryCalls.Load())
	}
	if f.rerunBodies[0] != `{"from_failed":true}` {
		t.Errorf("rerun body = %q, want from_failed", f.rerunBodies[0])
	}
	for _, tok := range f.circleTokens {
		if tok != "tok" {
			t.Errorf("CircleCI request without the Circle-Token header (got %q)", tok)
		}
	}
}

func TestClassifyCancelled_retryOneRerunPerWorkflow(t *testing.T) {
	// Two cancelled jobs of the same workflow: rerunning the workflow once
	// covers both, and a second rerun would restart the first job.
	f := &cancelledFixture{head: cxHeadCancelled, token: "tok", statuses: []circleStatus{
		{context: "ci/circleci: go-test", num: 1265, fixture: fixtureCancelledHead},
		{context: "ci/circleci: go-build", num: 1263, fixture: fixtureCancelledHead},
	}}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusRetried {
		t.Fatalf("state = %v (%s), want StatusRetried", got.State, got.Detail)
	}
	if got.Detail != "re-checking; workflow build rerun from failed" {
		t.Errorf("detail = %q", got.Detail)
	}
	if f.rerunCalls.Load() != 1 {
		t.Errorf("rerun calls = %d, want 1 for two jobs of one workflow", f.rerunCalls.Load())
	}
}

func TestClassifyCancelled_retryRerunsEachWorkflow(t *testing.T) {
	const otherWorkflow = "1f6d4b52-7e30-4a0f-9d4e-9f0f5c2b1a77"
	f := &cancelledFixture{head: cxHeadCancelled, token: "tok", statuses: []circleStatus{
		{context: "ci/circleci: go-test", num: 1265, fixture: fixtureCancelledHead, workflowID: otherWorkflow},
		{context: "ci/circleci: go-build", num: 1263, fixture: fixtureCancelledHead},
	}}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusRetried {
		t.Fatalf("state = %v (%s), want StatusRetried", got.State, got.Detail)
	}
	if f.rerunCalls.Load() != 2 {
		t.Errorf("rerun calls = %d, want 2 for two distinct workflows", f.rerunCalls.Load())
	}
}

func TestClassifyCancelled_retryFallsBackWithoutWorkflow(t *testing.T) {
	// A build outside a workflow has nothing to rerun from failed, so the
	// v1.1 single-build retry stays the remedy.
	f := &cancelledFixture{head: cxHeadCancelled, token: "tok", statuses: []circleStatus{
		{context: "ci/circleci: go-build", num: 1263, fixture: fixtureCancelledHead, noWorkflow: true},
	}}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusRetried {
		t.Fatalf("state = %v (%s), want StatusRetried", got.State, got.Detail)
	}
	if got.Detail != "re-checking; build 1263 retried as 1272" {
		t.Errorf("detail = %q", got.Detail)
	}
	if f.retryCalls.Load() != 1 || f.rerunCalls.Load() != 0 {
		t.Errorf("retries = %d, reruns = %d; want 1 and 0", f.retryCalls.Load(), f.rerunCalls.Load())
	}
}

func TestClassifyCancelled_rerunErrorKeepsCancelled(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead), token: "tok", rerunStatus: http.StatusForbidden}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled when the rerun is refused", got.State, got.Detail)
	}
	want := "build 1263 auto-cancelled; retry needed; rerun of workflow build failed: HTTP 403: Permission denied"
	if got.Detail != want {
		t.Errorf("detail = %q\nwant     %q", got.Detail, want)
	}
}

func TestClassifyCancelled_dryRunOnlyClassifies(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead), token: "tok"}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true; p.DryRun = true })

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled in dry run", got.State, got.Detail)
	}
	if f.retryCalls.Load() != 0 {
		t.Errorf("retry called %d times in dry run, want 0", f.retryCalls.Load())
	}
}

func TestClassifyCancelled_retryNeedsToken(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead)}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled", got.State, got.Detail)
	}
	if got.Detail != "build 1263 auto-cancelled; retry needed; retry skipped: no CircleCI token configured" {
		t.Errorf("detail = %q", got.Detail)
	}
	if f.retryCalls.Load() != 0 {
		t.Errorf("retry attempted %d times without a token, want 0", f.retryCalls.Load())
	}
}

func TestClassifyCancelled_privateWithoutTokenDegradesToFailed(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead), private: true}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed: the build could not be inspected", got.State, got.Detail)
	}
	want := "checks failed: ci/circleci: go-build; ci/circleci: go-build: build 1263 could not be inspected (HTTP 404: Build not found; no CircleCI token configured (private project?))"
	if got.Detail != want {
		t.Errorf("detail = %q\nwant     %q", got.Detail, want)
	}
	if f.retryCalls.Load() != 0 {
		t.Errorf("retry attempted %d times, want 0", f.retryCalls.Load())
	}
}

func TestClassifyCancelled_privateWithTokenIsInspected(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead), private: true, token: "tok"}
	got := f.run(t, nil)

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled", got.State, got.Detail)
	}
}

func TestClassifyCancelled_mixedWithRealCheckRunStaysFailed(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead), checkRunFailure: true, token: "tok"}
	got := f.run(t, func(p *Processor) { p.RetryCancelled = true })

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed: lint really failed", got.State, got.Detail)
	}
	if got.Detail != "checks failed: lint, ci/circleci: go-build" {
		t.Errorf("detail = %q", got.Detail)
	}
	if f.buildCalls.Load() != 0 {
		t.Errorf("CircleCI looked up %d times although a non-CircleCI check failed, want 0 (lazy)", f.buildCalls.Load())
	}
}

func TestClassifyCancelled_securityContextIsCancelledNotSecurity(t *testing.T) {
	// Decided before the security split, like stale: a cancelled gosec
	// build is not a security finding.
	f := &cancelledFixture{head: cxHeadCancelled, statuses: []circleStatus{{context: "ci/circleci: gosec", num: 1263, fixture: fixtureCancelledHead}}}
	got := f.run(t, nil)

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled", got.State, got.Detail)
	}
}

func TestClassifyCancelled_uninspectableSecurityContextKeepsNote(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: []circleStatus{{context: "ci/circleci: gosec", num: 1263, fixture: fixtureCancelledHead}}, private: true}
	got := f.run(t, nil)

	if got.State != pr.StatusFailedSecurity {
		t.Fatalf("state = %v (%s), want StatusFailedSecurity", got.State, got.Detail)
	}
	if !strings.HasPrefix(got.Detail, "security check failed: ci/circleci: gosec; ") || !strings.Contains(got.Detail, "could not be inspected") {
		t.Errorf("detail = %q, want the security detail plus the inspection note", got.Detail)
	}
}

func TestClassifyCancelled_decidedBeforeStale(t *testing.T) {
	// Behind main with go-build green there AND auto-cancelled: the
	// cancellation is the cause, staleness only a heuristic, so the PR is
	// Cancelled and the base branch is never consulted.
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead), behindBy: 12}
	got := f.run(t, func(p *Processor) { p.RefreshStale = true })

	if got.State != pr.StatusCancelled {
		t.Fatalf("state = %v (%s), want StatusCancelled", got.State, got.Detail)
	}
	if f.baseLookupCalls.Load() != 0 {
		t.Errorf("base branch looked up %d times, want 0", f.baseLookupCalls.Load())
	}
}

func TestClassifyCancelled_nilClientKeepsTodaysBehaviour(t *testing.T) {
	f := &cancelledFixture{head: cxHeadCancelled, statuses: goBuildStatus(1263, fixtureCancelledHead)}
	got := f.run(t, func(p *Processor) { p.CircleCI = nil })

	if got.State != pr.StatusFailed || got.Detail != "checks failed: ci/circleci: go-build" {
		t.Fatalf("state = %v (%s), want plain StatusFailed without a CircleCI client", got.State, got.Detail)
	}
	if f.buildCalls.Load() != 0 {
		t.Errorf("CircleCI called %d times without a client, want 0", f.buildCalls.Load())
	}
}

func TestCancelledResult_detail(t *testing.T) {
	ref := func(n int) circleci.BuildRef {
		return circleci.BuildRef{VCS: "github", Owner: "giantswarm", Repo: "mcp-capi", Num: n}
	}
	onHead := &cancelledResult{HeadSHA: cxHeadCancelled, Builds: []cancelledBuild{
		{Context: "ci/circleci: go-build", Ref: ref(1263), Revision: cxHeadCancelled},
		{Context: "ci/circleci: go-test", Ref: ref(1265), Revision: cxHeadCancelled},
	}}
	if !onHead.onHead() {
		t.Error("onHead() = false for builds on the head")
	}
	if got := onHead.detail(); got != "builds 1263, 1265 auto-cancelled; retry needed" {
		t.Errorf("detail() = %q", got)
	}

	behind := &cancelledResult{HeadSHA: cxHeadMoved, Builds: []cancelledBuild{
		{Context: "ci/circleci: go-build", Ref: ref(1244), Revision: "2c0ce64c2e56aeed9a3c15865243f539d6ab25f4"},
	}}
	if behind.onHead() {
		t.Error("onHead() = true for a build behind the head")
	}
	if got := behind.detail(); got != "build 1244 (2c0ce64) auto-cancelled; head is now 605a2d6, its build is the verdict" {
		t.Errorf("detail() = %q", got)
	}

	if (&cancelledResult{HeadSHA: cxHeadMoved}).onHead() {
		t.Error("onHead() = true with no builds")
	}
}

func TestWithNote(t *testing.T) {
	if got := withNote("checks failed: x", ""); got != "checks failed: x" {
		t.Errorf("withNote without note = %q", got)
	}
	if got := withNote("checks failed: x", "y"); got != "checks failed: x; y" {
		t.Errorf("withNote with note = %q", got)
	}
}
