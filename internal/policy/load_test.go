package policy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// loader serves files over the contents API of a fake giantswarm/github. A
// path that is not in files answers 404, the way GitHub answers both for a
// file that is not there and for a repository the token cannot read.
func loader(t *testing.T, files map[string]string) Loader {
	t.Helper()
	const owner, repo = "giantswarm", "github"
	prefix := fmt.Sprintf("/repos/%s/%s/contents/", owner, repo)

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix, func(w http.ResponseWriter, r *http.Request) {
		content, ok := files[strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		body := base64.StdEncoding.EncodeToString([]byte(content))
		kind, encoding := "file", "base64"
		_ = json.NewEncoder(w).Encode(github.RepositoryContent{Type: &kind, Encoding: &encoding, Content: &body})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return Loader{Client: client, Owner: owner, Repo: repo}
}

// TestTeamScope resolves the three files of a team scope and records which
// ones produced the policy.
func TestTeamScope(t *testing.T) {
	l := loader(t, map[string]string{
		DefaultFile:                   "updateTypes:\n  renovate: [patch, minor]\n",
		TeamFile("bumblebee"):         "slackChannel: team-bumblebee\nschedule: enabled\nconcurrency:\n  perRepo: 2\n",
		RepositoriesFile("bumblebee"): "- name: marge\n- name: muster\n  botPRsSweep:\n    updateTypes: [patch]\n",
	})

	scope, err := l.TeamScope(t.Context(), "bumblebee")
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/marge", "giantswarm/muster"}, scope.Repos)

	base := scope.Policies.Base()
	require.Equal(t, "team-bumblebee", base.SlackChannel)
	require.Equal(t, 2, base.Concurrency.PerRepo)
	require.True(t, base.Schedule, "the team file opts the team into the schedule")
	require.Equal(t, []string{"built-in company defaults", DefaultFile, TeamFile("bumblebee")}, base.Sources)

	require.True(t, scope.Policies.For("marge").Eligible(pr.KindRenovate, pr.UpdateMinor))
	require.False(t, scope.Policies.For("muster").Eligible(pr.KindRenovate, pr.UpdateMinor), "the exception narrows to patch")
}

// TestTeamScope_noPolicyFiles falls back to the company defaults and says
// so in the sources, which is the one fallback the loader allows.
func TestTeamScope_noPolicyFiles(t *testing.T) {
	l := loader(t, map[string]string{RepositoriesFile("shield"): "- name: cluster-aws\n"})

	scope, err := l.TeamScope(t.Context(), "shield")
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/cluster-aws"}, scope.Repos)
	require.Equal(t, pr.CompanyDefaults(), scope.Policies.For("cluster-aws"))
	require.False(t, scope.Policies.Base().Schedule, "a team without a policy file is never swept by the schedule")
}

// TestTeamScope_unreadableRepository names the access in the error. The
// repository list is read first for this reason: GitHub answers 404 for a
// repository the token cannot read, so without it every file looks absent
// and the sweep would run on the company defaults.
func TestTeamScope_unreadableRepository(t *testing.T) {
	l := loader(t, nil)

	_, err := l.TeamScope(t.Context(), "bumblebee")
	require.ErrorContains(t, err, `no team file for "bumblebee"`)
	require.ErrorContains(t, err, "the token cannot read giantswarm/github")
}

// TestTeamScope_malformedFileStops refuses to sweep under a policy the team
// never wrote, and names the file and the key.
func TestTeamScope_malformedFileStops(t *testing.T) {
	l := loader(t, map[string]string{
		DefaultFile:                   "shedule: enabled\n",
		RepositoriesFile("bumblebee"): "- name: marge\n",
	})

	_, err := l.TeamScope(t.Context(), "bumblebee")
	require.ErrorContains(t, err, DefaultFile)
	require.ErrorContains(t, err, "field shedule not found")
}

// TestTeamScope_malformedExceptionStops holds a repository exception to the
// same standard as a policy file.
func TestTeamScope_malformedExceptionStops(t *testing.T) {
	l := loader(t, map[string]string{
		RepositoriesFile("bumblebee"): "- name: marge\n  botPRsSweep:\n    updateTypes: [everything]\n",
	})

	_, err := l.TeamScope(t.Context(), "bumblebee")
	require.ErrorContains(t, err, RepositoriesFile("bumblebee"))
	require.ErrorContains(t, err, `unknown update type "everything"`)
}

// TestQueryScope reads the company defaults alone: the query scope has no
// team, so no team file and no repository exception apply to it.
func TestQueryScope(t *testing.T) {
	l := loader(t, map[string]string{DefaultFile: "concurrency:\n  perTeam: 2\n"})

	scope, err := l.QueryScope(t.Context())
	require.NoError(t, err)
	require.Nil(t, scope.Repos, "the query scope leaves the PRs to the GitHub search")
	require.Equal(t, 2, scope.Policies.Base().Concurrency.PerTeam)
	require.Equal(t, []string{"built-in company defaults", DefaultFile}, scope.Policies.Base().Sources)
}

// TestTeamScope_emptyRepositoryList refuses a scope with no repository in
// it, which would otherwise sweep nothing and report success.
func TestTeamScope_emptyRepositoryList(t *testing.T) {
	l := loader(t, map[string]string{RepositoriesFile("shield"): "[]\n"})

	_, err := l.TeamScope(t.Context(), "shield")
	require.ErrorContains(t, err, "lists no repositories")
}
