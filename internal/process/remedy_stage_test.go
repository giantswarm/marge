package process

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
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
			Head:           &github.PullRequestBranch{SHA: new(stageHead), Ref: new("renovate/x-net"), Repo: &github.Repository{FullName: new("org/repo")}},
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
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_pull_request_reviews", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
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

// gateServer answers ProcessPR for a Renovate PR whose only wait is a
// Heimdall gate naming a suite nobody started. behindBy is how far the head
// is behind main, and a negative value fails the comparison. Every write is
// recorded as "METHOD path body".
func gateServer(t *testing.T, behindBy int, writes *[]string) *httptest.Server {
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
			MergeableState: new("blocked"),
			User:           &github.User{Login: new("renovate[bot]")},
			Head:           &github.PullRequestBranch{SHA: new(stageHead), Ref: new("renovate/x-net"), Repo: &github.Repository{FullName: new("org/repo")}},
			Base:           &github.PullRequestBranch{SHA: new(stageBase), Ref: new("main")},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.CombinedStatus{State: new("pending"), SHA: new(stageHead)})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		write(w, github.ListCheckRunsResults{Total: new(1), CheckRuns: []*github.CheckRun{{
			ID: new(int64(9)), Name: new("Heimdall - PR Gatekeeper"), Status: new("in_progress"),
			Output: &github.CheckRunOutput{Title: new("Heimdall - PR Gatekeeper"), Text: new(heimdallText)},
		}}})
	})
	mux.HandleFunc("GET /repos/org/repo/compare/main..."+stageHead, func(w http.ResponseWriter, r *http.Request) {
		if behindBy < 0 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		write(w, github.CommitsComparison{
			Status: new("diverged"), BehindBy: new(behindBy), AheadBy: new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(stageBase)},
		})
	})
	for _, path := range []string{
		"GET /repos/org/repo/branches/main/protection/required_pull_request_reviews",
		"GET /repos/org/repo/rules/branches/main",
		"GET /repos/org/repo/branches/main/protection/required_status_checks",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	}
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, _ *http.Request) {
		write(w, []*github.IssueComment{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			*writes = append(*writes, r.Method+" "+r.URL.Path+" "+body.Body)
		}
		if r.Method == http.MethodPut && r.URL.Path == "/repos/org/repo/pulls/1/update-branch" {
			w.WriteHeader(http.StatusAccepted)
		}
		write(w, struct{}{})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// gateProcessor runs the shipped Heimdall rule with the default actions.
func gateProcessor(t *testing.T, srv *httptest.Server) *Processor {
	t.Helper()
	dir := t.TempDir()
	doc, err := os.ReadFile(filepath.Join("..", "..", "rules", "heimdall-suite-not-triggered.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "heimdall-suite-not-triggered.yaml"), doc, 0o600))
	cat, err := rules.Loader{LocalPath: dir}.Load(t.Context(), remedy.Default())
	require.NoError(t, err)
	require.Empty(t, cat.Skipped)

	proc := NewProcessor(newTestClient(t, srv), false, false, "marge")
	proc.Actions = ActionSet{ActionClassify: true, ActionRemedy: true, ActionMark: true}
	proc.Rules = cat
	proc.Remedies = remedy.Default()
	return proc
}

// A PR waiting on the gate alone gets the gate's command when it is up to
// date, and a branch update when it is behind, so no PR waits on a bot that
// rebases only on conflict. A comparison that cannot be had writes nothing.
func TestProcessPRGateWaitsForAnUpToDateBranch(t *testing.T) {
	const command = "/run app-test-suites-single PROVIDER=capa"
	cases := map[string]struct {
		behindBy  int
		state     pr.StatusState
		commented bool
		updated   bool
	}{
		"up to date":   {behindBy: 0, state: pr.StatusWaitingChecks, commented: true},
		"behind":       {behindBy: 2, state: pr.StatusRefreshed, updated: true},
		"not compared": {behindBy: -1, state: pr.StatusWaitingChecks},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var writes []string
			proc := gateProcessor(t, gateServer(t, tc.behindBy, &writes))

			status := pr.NewPRStatus()
			info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1, Author: "renovate[bot]"}
			idx := status.Add(info)

			proc.ProcessPR(t.Context(), info, status, idx)

			require.Equal(t, tc.state, status.StateAt(idx), "%+v", status.Snapshot()[idx])
			commented, updated := false, false
			for _, w := range writes {
				commented = commented || w == "POST /repos/org/repo/issues/1/comments "+command
				updated = updated || strings.HasPrefix(w, "PUT /repos/org/repo/pulls/1/update-branch")
			}
			require.Equal(t, tc.commented, commented, "writes: %v", writes)
			require.Equal(t, tc.updated, updated, "writes: %v", writes)
		})
	}
}
