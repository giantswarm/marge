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
	mux.HandleFunc("POST /graphql", graphQLPulls(t, map[string][]*github.PullRequest{
		"org/ok": {
			{Number: new(1), Title: new("chore(deps): update x"), HTMLURL: new("https://github.com/org/ok/pull/1"), User: &github.User{Login: new("renovate[bot]")}, Labels: []*github.Label{{Name: "dependencies"}, {Name: "marge/action-required"}}},
			{Number: new(2), Title: new("feat: by a person"), HTMLURL: new("https://github.com/org/ok/pull/2"), User: &github.User{Login: new("quentin")}},
			{Number: new(3), Title: new("chore: align files"), HTMLURL: new("https://github.com/org/ok/pull/3"), User: &github.User{Login: new("giantswarm-align-files[bot]")}},
		},
	}, map[string]string{"org/broken": "boom"}))
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)

	found, err := searchPRs(t.Context(), client, "", "me", []string{"org/ok", "org/broken"})
	require.NoError(t, err)
	require.Len(t, found.PRs, 2)
	for _, p := range found.PRs {
		require.NotEqual(t, "", pr.DiscoveredKind(p), p.Author)
	}
	require.Equal(t, []string{"dependencies", "marge/action-required"}, found.PRs[0].Labels,
		"the labels carry the classification a previous sweep stored")
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
	status.SetLabel(idx, "marge/merged")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusSkipped, "dry-run: would approve, merge (squash)")
	idx3 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 3})
	status.Update(idx3, pr.StatusWaitingChecks, "required checks pending: go-build")

	got := buildSweepResult(status, []repoFailure{{Repo: "o/broken", Err: "boom"}}, nil)

	require.Len(t, got.Merged, 1)
	require.Equal(t, "marge/merged", got.Merged[0].Label)
	require.Equal(t, "renovate", got.Merged[0].Kind)
	require.Equal(t, "patch", got.Merged[0].UpdateType)
	require.Len(t, got.Skipped, 1)
	require.Empty(t, got.Skipped[0].Label, "nothing was written in the dry run")
	require.Len(t, got.Waiting, 1)
	require.Equal(t, 1, got.Summary.Waiting)
	require.Equal(t, []SweepRepoFailure{{Repo: "o/broken", Error: "boom"}}, got.RepositoriesFailed)
}

// TestSearchPRs_carriesTheLabels: the GitHub search reports the labels of
// every issue it returns, so the search path carries the stored
// classification exactly as the repository listing does.
func TestSearchPRs_carriesTheLabels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{
			Issues: []*github.Issue{{
				Number:  new(1),
				Title:   new("chore(deps): update x"),
				HTMLURL: new("https://github.com/org/ok/pull/1"),
				User:    &github.User{Login: new("renovate[bot]")},
				Labels:  []*github.Label{{Name: "marge/stale"}},
			}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)

	found, err := searchPRs(t.Context(), client, "", "me", nil)
	require.NoError(t, err)
	require.Len(t, found.PRs, 1, "the same PR is returned by every query and kept once")
	require.Equal(t, []string{"marge/stale"}, found.PRs[0].Labels)
}

// TestListRepoPRs_selfHostedRenovate: in a personal repository the owner's
// PR from a Renovate branch is a self-hosted Renovate candidate; the owner's
// other PRs and a collaborator's Renovate-named branch are not.
func TestListRepoPRs_selfHostedRenovate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", graphQLPulls(t, map[string][]*github.PullRequest{
		"jane/app": {
			{Number: new(1), Title: new("chore(deps): update x to v1.2.3"), HTMLURL: new("https://github.com/jane/app/pull/1"), User: &github.User{Login: new("jane")}, Head: &github.PullRequestBranch{Ref: new("renovate/x-1.x")}},
			{Number: new(2), Title: new("feat: by the owner"), HTMLURL: new("https://github.com/jane/app/pull/2"), User: &github.User{Login: new("jane")}, Head: &github.PullRequestBranch{Ref: new("feat")}},
			{Number: new(3), Title: new("chore(deps): update y"), HTMLURL: new("https://github.com/jane/app/pull/3"), User: &github.User{Login: new("quentin")}, Head: &github.PullRequestBranch{Ref: new("renovate/y")}},
		},
	}, nil))
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)

	found, err := searchPRs(t.Context(), client, "", "me", []string{"jane/app"})
	require.NoError(t, err)
	require.Len(t, found.PRs, 1)
	require.Equal(t, 1, found.PRs[0].Number)
	require.Equal(t, "renovate/x-1.x", found.PRs[0].HeadRef)
}

// TestSearchPRs_searchesTheCallersSelfHostedRenovate: the search also asks
// for the caller's own PRs from Renovate branches in the caller's own
// repositories, where a self-hosted Renovate running as the caller opens them.
func TestSearchPRs_searchesTheCallersSelfHostedRenovate(t *testing.T) {
	var queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("q"))
		_ = json.NewEncoder(w).Encode(&github.IssuesSearchResult{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)

	_, err = searchPRs(t.Context(), client, "", "jane", nil)
	require.NoError(t, err)
	require.Contains(t, queries, "is:pr is:open archived:false user:jane author:jane head:renovate/")
	require.Contains(t, queries, "is:pr is:open archived:false user:jane author:app/renovate")
}
