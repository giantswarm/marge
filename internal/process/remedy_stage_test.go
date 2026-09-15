package process

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
)

const (
	stageHead = "headsha0000000000000000000000000000000"
	stageBase = "basesha0000000000000000000000000000000"
)

// stageServer answers everything ProcessPR asks about one failing Renovate
// PR whose base head reported nothing. Anything the sweep writes is
// answered without comment, so the test asserts on the remedy alone.
func stageServer(t *testing.T) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.PullRequest{
			Number:         new(1),
			State:          new("open"),
			Title:          new("chore(deps): update module golang.org/x/net to v0.46.0"),
			MergeableState: new("clean"),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(stageHead), Ref: new("renovate/x-net")},
			Base:           &github.PullRequestBranch{SHA: new(stageBase), Ref: new("main")},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.CombinedStatus{State: new("failure"), SHA: new(stageHead)})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.ListCheckRunsResults{Total: new(1), CheckRuns: []*github.CheckRun{{
			ID: new(int64(9)), Name: new("go-build"),
			Status: new("completed"), Conclusion: new("failure"),
			DetailsURL: new("https://github.com/org/repo/actions/runs/7/job/9"),
		}}})
	})
	mux.HandleFunc("GET /repos/org/repo/compare/main..."+stageHead, func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.CommitsComparison{
			Status: new("ahead"), BehindBy: new(0), AheadBy: new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(stageBase)},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+stageBase+"/status", func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.CombinedStatus{State: new("success"), SHA: new(stageBase)})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+stageBase+"/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.ListCheckRunsResults{Total: new(0)})
	})
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, _ *http.Request) {
		write(w, []*github.IssueComment{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { write(w, struct{}{}) })

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The rule stage runs on the way out of ProcessPR, not on a path a caller
// selects, so a new early return cannot skip the catalogue. This drives a
// whole PR through ProcessPR: delete the applyRule call from finish, or
// return before the deferred finish is armed, and the recording action
// never runs.
func TestProcessPRReachesTheRuleStage(t *testing.T) {
	var requests []*remedy.Request
	srv := stageServer(t)

	proc := NewProcessor(newTestClient(t, srv), false, false, "marge")
	proc.Actions = ActionSet{ActionClassify: true, ActionRemedy: true, ActionMark: true}
	proc.Rules = ruleFor(t, "failed")
	proc.Remedies = remedy.NewRegistry(recordingAction{name: remedy.UpdateBranch, requests: &requests})

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1, Author: "renovate[bot]"}
	idx := status.Add(info)

	proc.ProcessPR(t.Context(), info, status, idx)

	require.Len(t, requests, 1, "ProcessPR ended without running the rule stage")
	require.Equal(t, "go-build", requests[0].Check)
	require.Equal(t, pr.StatusRemedied, status.StateAt(idx))
}
