package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// TestListRepoPRs_reportsUnlistableRepositories: a repository whose PRs
// cannot be listed appears under Failed, the others are still swept, and
// only the four bots' PRs are kept.
func TestListRepoPRs_reportsUnlistableRepositories(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/ok/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]*github.PullRequest{
			{Number: new(1), Title: new("chore(deps): update x"), HTMLURL: new("https://github.com/org/ok/pull/1"), User: &github.User{Login: new("renovate[bot]")}},
			{Number: new(2), Title: new("feat: by a person"), HTMLURL: new("https://github.com/org/ok/pull/2"), User: &github.User{Login: new("quentin")}},
			{Number: new(3), Title: new("chore: align files"), HTMLURL: new("https://github.com/org/ok/pull/3"), User: &github.User{Login: new("giantswarm-align-files[bot]")}},
		})
	})
	mux.HandleFunc("GET /repos/org/broken/pulls", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)

	found, err := searchPRs(t.Context(), client, "", "me", []string{"org/ok", "org/broken"})
	require.NoError(t, err)
	require.Len(t, found.PRs, 2)
	for _, p := range found.PRs {
		require.NotEqual(t, "", pr.KindOf(p.Author), p.Author)
	}
	require.Len(t, found.Failed, 1)
	require.Equal(t, "org/broken", found.Failed[0].Repo)
	require.Contains(t, found.Failed[0].Err, "boom")

	filtered, err := searchPRs(t.Context(), client, "ORG/OK", "me", []string{"org/ok", "org/broken"})
	require.NoError(t, err)
	require.Len(t, filtered.PRs, 2, "the query filters repositories by name, case-insensitively")
	require.Empty(t, filtered.Failed)
}

// TestBuildSweepResult_labelAndFailedRepositories: the JSON carries the
// label that is on the PR, nothing when none was written, and the
// repositories the sweep could not list.
func TestBuildSweepResult_labelAndFailedRepositories(t *testing.T) {
	status := pr.NewPRStatus()
	idx := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx, pr.StatusMerged, "squash")
	status.SetClassification(idx, pr.KindRenovate, pr.UpdatePatch)
	status.SetLabel(idx, "bot-prs-sweep/merged")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusSkipped, "dry-run: would approve, merge (squash)")
	idx3 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 3})
	status.Update(idx3, pr.StatusWaitingChecks, "required checks pending: go-build")

	got := buildSweepResult(status, []repoFailure{{Repo: "o/broken", Err: "boom"}})

	require.Len(t, got.Merged, 1)
	require.Equal(t, "bot-prs-sweep/merged", got.Merged[0].Label)
	require.Equal(t, "renovate", got.Merged[0].Kind)
	require.Equal(t, "patch", got.Merged[0].UpdateType)
	require.Len(t, got.Skipped, 1)
	require.Empty(t, got.Skipped[0].Label, "nothing was written in the dry run")
	require.Len(t, got.Waiting, 1)
	require.Equal(t, 1, got.Summary.Waiting)
	require.Equal(t, []SweepRepoFailure{{Repo: "o/broken", Error: "boom"}}, got.RepositoriesFailed)
}
