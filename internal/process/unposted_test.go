package process

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
)

// The unposted fixtures model giantswarm/tunnelport#131 (2026-10-05):
// CircleCI pipeline 483 for the head ran setup and build to success, and
// GitHub carried none of the required "ci/circleci: <job>" contexts, only
// the Actions checks.
const (
	upHead       = "de4e48ed1f910afd8c13d01ac291e35a90afece6"
	upOlderHead  = "5a0b6e39f4349c0be32ccc0d022338259e549298"
	upBranch     = "renovate/k8s-modules"
	upPipeline   = "045284af-b742-477f-aea5-6a754fb43ac3"
	upBuildWF    = "dd4e1b29-1015-4343-81f6-47f713b17ff0"
	upBuildRerun = "4ea38518-c4ad-47fd-86a3-4d3a021fc780"
	upSetupWF    = "dc65be3c-5812-4497-911d-52e8bb7f53b1"
)

// unpostedFixture wires a fake GitHub API for org/repo#1, whose base
// protection requires two CircleCI contexts and an Actions check, with only
// the Actions check reported on the head; and a fake CircleCI v2 API serving
// the head's pipelines, their workflows and jobs.
type unpostedFixture struct {
	// pipelines are the branch's pipelines, newest first, as revisions.
	pipelines []string
	// workflows are the newest pipeline's workflow runs, newest first.
	workflows []circleci.PipelineWorkflow
	// settled is how long ago the Actions check completed.
	settled time.Duration
	token   string

	mu          sync.Mutex
	rerunPaths  []string
	rerunBodies []string
	branches    []string
}

func (f *unpostedFixture) github(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.PullRequest{
			Number:         new(1),
			Title:          new("Update k8s modules"),
			MergeableState: new("blocked"),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(upHead), Ref: new(upBranch), Repo: &github.Repository{FullName: new("org/repo")}},
			Base:           &github.PullRequestBranch{SHA: new(fxBase), Ref: new("main")},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.CombinedStatus{State: new("pending"), SHA: new(upHead)})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.ListCheckRunsResults{Total: new(1), CheckRuns: []*github.CheckRun{{
			ID: new(int64(11)), Name: new("lint"),
			Status: new("completed"), Conclusion: new("success"),
			CompletedAt: &github.Timestamp{Time: time.Now().Add(-f.settled)},
		}}})
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.RequiredStatusChecks{Contexts: &[]string{"ci/circleci: go-build", "ci/circleci: go-test", "lint"}})
	})
	mux.HandleFunc("GET /repos/org/repo/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []*github.IssueComment{})
	})
	mux.HandleFunc("POST /repos/org/repo/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []*github.Label{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux)
}

func (f *unpostedFixture) circle(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	items := func(w http.ResponseWriter, v any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": v, "next_page_token": nil})
	}
	mux.HandleFunc("GET /api/v2/project/gh/org/repo/pipeline", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.branches = append(f.branches, r.URL.Query().Get("branch"))
		f.mu.Unlock()
		var ps []map[string]any
		for i, rev := range f.pipelines {
			id := upPipeline
			if i > 0 {
				id = "older-" + rev
			}
			ps = append(ps, map[string]any{"id": id, "number": 483 - i, "vcs": map[string]string{"revision": rev, "branch": upBranch}})
		}
		items(w, ps)
	})
	mux.HandleFunc("GET /api/v2/pipeline/"+upPipeline+"/workflow", func(w http.ResponseWriter, r *http.Request) {
		items(w, f.workflows)
	})
	mux.HandleFunc("GET /api/v2/workflow/{id}/job", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case upSetupWF:
			items(w, []circleci.WorkflowJob{{Name: "setup", Status: "success"}})
		default:
			items(w, []circleci.WorkflowJob{{Name: "go-test", Status: "success"}, {Name: "go-build", Status: "success"}})
		}
	})
	mux.HandleFunc("POST /api/v2/workflow/{id}/rerun", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Circle-Token") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Permission denied"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.rerunPaths = append(f.rerunPaths, r.URL.Path)
		f.rerunBodies = append(f.rerunBodies, string(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"workflow_id":"9b9c4a0e-0000-4000-8000-000000000002"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected CircleCI request: %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux)
}

func (f *unpostedFixture) run(t *testing.T, actions ActionSet, dryRun bool) pr.StatusEntry {
	t.Helper()
	gh := f.github(t)
	defer gh.Close()
	cc := f.circle(t)
	defer cc.Close()

	proc := NewProcessor(newTestClient(t, gh), dryRun, false, "me")
	proc.CircleCI = &circleci.Client{HTTPClient: cc.Client(), BaseURL: cc.URL, Token: f.token}
	proc.Actions = actions
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1, Author: "renovate[bot]"}
	idx := status.Add(info)
	proc.ProcessPR(context.Background(), info, status, idx)
	return status.Snapshot()[idx]
}

// finishedBuild is pipeline 483 as CircleCI reported it before anyone
// reran it: setup and build both succeeded.
func finishedBuild() []circleci.PipelineWorkflow {
	return []circleci.PipelineWorkflow{
		{ID: upBuildWF, Name: "build", Status: "success"},
		{ID: upSetupWF, Name: "setup", Status: "success", Tag: "setup"},
	}
}

var retryActions = ActionSet{ActionClassify: true, ActionRetry: true}

const upWaiting = "required checks not reported: ci/circleci: go-build, ci/circleci: go-test"

func TestUnposted_finishedWorkflowIsRerunOnce(t *testing.T) {
	f := &unpostedFixture{pipelines: []string{upHead}, workflows: finishedBuild(), settled: time.Hour, token: "tok"}
	got := f.run(t, retryActions, false)

	if got.State != pr.StatusRetried {
		t.Fatalf("state = %v (%s), want StatusRetried", got.State, got.Detail)
	}
	if want := "re-checking; workflow build (pipeline 483) rerun (statuses not posted)"; got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
	// The workflow that ran the missing jobs, from the beginning: it
	// succeeded, so there is no failed job to rerun from.
	if len(f.rerunPaths) != 1 || f.rerunPaths[0] != "/api/v2/workflow/"+upBuildWF+"/rerun" {
		t.Fatalf("reruns = %q, want one of the build workflow", f.rerunPaths)
	}
	if f.rerunBodies[0] != `{"from_failed":false}` {
		t.Errorf("rerun body = %q, want a full rerun", f.rerunBodies[0])
	}
	if len(f.branches) != 1 || f.branches[0] != upBranch {
		t.Errorf("pipelines read for branches %q, want %q", f.branches, upBranch)
	}
}

// TestUnposted_oncePerHead: CircleCI records the sweep's rerun as a workflow
// run tagged as a rerun in the head's pipeline. A rerun that again posted
// nothing is not rerun a second time; the PR is reported without a verdict
// instead of waiting for ever.
func TestUnposted_oncePerHead(t *testing.T) {
	workflows := append([]circleci.PipelineWorkflow{
		{ID: upBuildRerun, Name: "build", Status: "success", Tag: "rerun-workflow-from-beginning"},
	}, finishedBuild()...)
	f := &unpostedFixture{pipelines: []string{upHead}, workflows: workflows, settled: time.Hour, token: "tok"}
	got := f.run(t, retryActions, false)

	if len(f.rerunPaths) != 0 {
		t.Fatalf("reruns = %q, want none: the head was rerun once already", f.rerunPaths)
	}
	if got.State != pr.StatusNoVerdict {
		t.Fatalf("state = %v (%s), want StatusNoVerdict", got.State, got.Detail)
	}
	if want := upWaiting + "; workflow build (pipeline 483) rerun once on this head, statuses not posted again"; got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
}

// TestUnposted_newHeadIsRerunAgain: the rerun of an older head does not
// count for a new one, whose own pipeline is rerun.
func TestUnposted_newHeadIsRerunAgain(t *testing.T) {
	f := &unpostedFixture{pipelines: []string{upHead, upOlderHead}, workflows: finishedBuild(), settled: time.Hour, token: "tok"}
	got := f.run(t, retryActions, false)

	if got.State != pr.StatusRetried || len(f.rerunPaths) != 1 {
		t.Fatalf("state = %v (%s), reruns %q; want the new head's workflow rerun", got.State, got.Detail, f.rerunPaths)
	}
}

func TestUnposted_noPipelineForHead(t *testing.T) {
	f := &unpostedFixture{pipelines: []string{upOlderHead}, settled: time.Hour, token: "tok"}
	got := f.run(t, retryActions, false)

	if got.State != pr.StatusNoVerdict {
		t.Fatalf("state = %v (%s), want StatusNoVerdict", got.State, got.Detail)
	}
	if want := upWaiting + "; CircleCI has no pipeline for head de4e48e, push a commit to start one"; got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
}

func TestUnposted_runningWorkflowKeepsWaiting(t *testing.T) {
	workflows := finishedBuild()
	workflows[0].Status = "running"
	f := &unpostedFixture{pipelines: []string{upHead}, workflows: workflows, settled: time.Hour, token: "tok"}
	got := f.run(t, retryActions, false)

	if got.State != pr.StatusWaitingChecks || got.Detail != upWaiting || len(f.rerunPaths) != 0 {
		t.Fatalf("state = %v (%s), reruns %q; want a plain wait", got.State, got.Detail, f.rerunPaths)
	}
}

func TestUnposted_freshlySettledHeadKeepsWaiting(t *testing.T) {
	f := &unpostedFixture{pipelines: []string{upHead}, workflows: finishedBuild(), settled: time.Minute, token: "tok"}
	got := f.run(t, retryActions, false)

	if got.State != pr.StatusWaitingChecks || len(f.branches) != 0 {
		t.Fatalf("state = %v (%s), CircleCI read for %q; want a wait without a lookup", got.State, got.Detail, f.branches)
	}
}

func TestUnposted_withoutRetryOnlyReports(t *testing.T) {
	for name, tc := range map[string]struct {
		actions ActionSet
		dryRun  bool
		token   string
		suffix  string
	}{
		"retry not selected": {ActionSet{ActionClassify: true}, false, "tok", "rerun needed"},
		"dry run":            {retryActions, true, "tok", "rerun needed"},
		"no token":           {retryActions, false, "", "rerun skipped: no CircleCI token configured"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &unpostedFixture{pipelines: []string{upHead}, workflows: finishedBuild(), settled: time.Hour, token: tc.token}
			got := f.run(t, tc.actions, tc.dryRun)

			if len(f.rerunPaths) != 0 {
				t.Fatalf("reruns = %q, want none", f.rerunPaths)
			}
			want := upWaiting + "; workflow build (pipeline 483) finished, statuses not posted; " + tc.suffix
			if got.State != pr.StatusNoVerdict || got.Detail != want {
				t.Errorf("got %v (%s), want StatusNoVerdict (%s)", got.State, got.Detail, want)
			}
		})
	}
}

func TestCircleBranch_fork(t *testing.T) {
	run := &prRun{
		info: pr.PRInfo{Owner: "org", Repo: "repo", Number: 7},
		pull: &github.PullRequest{Head: &github.PullRequestBranch{Ref: new("main"), Repo: &github.Repository{FullName: new("someone/repo")}}},
	}
	if got := circleBranch(run); got != "pull/7" {
		t.Errorf("circleBranch = %q, want pull/7", got)
	}
	run.pull.Head.Repo.FullName = new("org/repo")
	if got := circleBranch(run); got != "main" {
		t.Errorf("circleBranch = %q, want main", got)
	}
}
