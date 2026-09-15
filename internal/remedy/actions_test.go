package remedy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
)

func apiClient(t *testing.T, server *httptest.Server) *github.Client {
	t.Helper()
	baseURL := server.URL + "/"
	client, err := github.NewClient(
		github.WithHTTPClient(server.Client()),
		github.WithURLs(&baseURL, &baseURL),
	)
	require.NoError(t, err)
	return client
}

func botRequest(client *github.Client) *Request {
	return &Request{
		Info: pr.PRInfo{Owner: "giantswarm", Repo: "marge", Number: 1},
		Pull: &github.PullRequest{
			User: &github.User{Login: new("renovate[bot]")},
			Base: &github.PullRequestBranch{Ref: new("main")},
		},
		Kind:       "renovate",
		LogMatched: true,
		Deps:       Deps{GitHub: client},
	}
}

func TestDefaultRegistryNames(t *testing.T) {
	require.Equal(t, []Name{
		CircleCIRetry,
		Close,
		DispatchAlignWorkflow,
		FixProtectionContext,
		MarkWait,
		RerunFailed,
		StrictChain,
		UpdateBranch,
	}, Default().Names())
}

func TestUpdateBranchAppliesOn202(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"message": "Updating pull request branch."}`))
	}))
	defer server.Close()

	out, err := Default().Apply(t.Context(), UpdateBranch, botRequest(apiClient(t, server)), nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.Equal(t, "branch updated from main", out.Detail)
	require.Equal(t, "/repos/giantswarm/marge/pulls/1/update-branch", path)
}

// update-branch acts on a branch that is behind, which the log says nothing
// about, so it is the one action that asks for no excerpt.
func TestUpdateBranchNeedsNoLogExcerpt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	req := botRequest(apiClient(t, server))
	req.LogMatched = false

	out, err := Default().Apply(t.Context(), UpdateBranch, req, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
}

// Every other action refuses without one: a check name is not a diagnosis.
func TestRerunFailedRefusesWithoutALogExcerpt(t *testing.T) {
	req := botRequest(nil)
	req.LogMatched = false
	req.CheckURL = "https://github.com/giantswarm/marge/actions/runs/77/job/88"

	out, err := Default().Apply(t.Context(), RerunFailed, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "check name alone")
}

func TestRerunFailedRerunsTheRun(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	req := botRequest(apiClient(t, server))
	req.Check = "go-build"
	req.CheckURL = "https://github.com/giantswarm/marge/actions/runs/77/job/88"

	out, err := Default().Apply(t.Context(), RerunFailed, req, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.Equal(t, "failed jobs of run 77 rerun", out.Detail)
	require.Equal(t, "/repos/giantswarm/marge/actions/runs/77/rerun-failed-jobs", path)
}

func TestRerunFailedWithoutARunInTheURL(t *testing.T) {
	req := botRequest(nil)
	req.Check = "ci/circleci: go-build"
	req.CheckURL = "https://circleci.com/gh/giantswarm/marge/12"

	out, err := Default().Apply(t.Context(), RerunFailed, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Equal(t, "no Actions run behind ci/circleci: go-build", out.Refused)
}

func TestCircleCIRetryRerunsTheWorkflow(t *testing.T) {
	var rerun string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			rerun = r.URL.Path
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"build_num": 12, "workflows": {"workflow_id": "abc", "workflow_name": "build"}}`))
	}))
	defer server.Close()

	req := botRequest(nil)
	req.Check = "ci/circleci: go-build"
	req.CheckURL = "https://circleci.com/gh/giantswarm/marge/12"
	req.Deps.CircleCI = &circleci.Client{HTTPClient: server.Client(), BaseURL: server.URL, Token: "t"}

	out, err := Default().Apply(t.Context(), CircleCIRetry, req, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.Equal(t, "workflow build rerun from failed", out.Detail)
	require.Contains(t, rerun, "abc")
}

func TestCircleCIRetryWithoutAToken(t *testing.T) {
	req := botRequest(nil)
	req.CheckURL = "https://circleci.com/gh/giantswarm/marge/12"

	out, err := Default().Apply(t.Context(), CircleCIRetry, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "no CircleCI token")
}

// An action that already ran against this change does not run again: twice
// red on the same code is a real failure.
func TestActionRunsOncePerChange(t *testing.T) {
	req := botRequest(nil)
	req.Check = "go-build"
	req.CheckURL = "https://github.com/giantswarm/marge/actions/runs/77/job/88"
	req.AppliedThisChange = map[Name]bool{RerunFailed: true}

	out, err := Default().Apply(t.Context(), RerunFailed, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Equal(t, "rerun-failed already applied to this change", out.Refused)
}

// A refresh neither merges nor rescues, so a failing security check does not
// stop it. The stale case depends on this: a scan the base branch has
// already fixed is exactly what a refresh is for.
func TestUpdateBranchRunsWithAFailingSecurityCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	req := botRequest(apiClient(t, server))
	req.SecurityFailure = "govulncheck"

	out, err := Default().Apply(t.Context(), UpdateBranch, req, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
}

// Every other action stops there: twice red on the same code is real, and a
// rerun of a failing scan only hides it.
func TestRerunFailedRefusesWithAFailingSecurityCheck(t *testing.T) {
	req := botRequest(nil)
	req.SecurityFailure = "govulncheck"
	req.CheckURL = "https://github.com/giantswarm/marge/actions/runs/77/job/88"

	out, err := Default().Apply(t.Context(), RerunFailed, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Equal(t, "security check failed: govulncheck", out.Refused)
}
