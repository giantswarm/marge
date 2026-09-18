package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

// storedServer serves the two calls the stored read makes -- the login and
// the repository listing -- and records every path it is asked for, so a
// test can hold what the read did NOT call.
func storedServer(t *testing.T, pulls []*github.PullRequest) (toolset, *[]string) {
	t.Helper()

	var (
		mu    sync.Mutex
		paths []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(&github.User{Login: new("me")})
		case "/graphql":
			graphQLPulls(t, map[string][]*github.PullRequest{"org/one": pulls}, nil)(w, r)
		default:
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return toolset{newClient: func(context.Context) (*github.Client, error) { return client, nil }}, &paths
}

func listResult(t *testing.T, tools toolset, arguments map[string]any) SweepResult {
	t.Helper()
	got, err := tools.handleList(t.Context(), call(arguments))
	require.NoError(t, err)
	require.False(t, got.IsError, textOf(t, got))
	var result SweepResult
	require.NoError(t, json.Unmarshal([]byte(textOf(t, got)), &result))
	return result
}

// TestList_readsTheStoredClassification is what #126 buys: the default list
// reports the classification the last sweep wrote on each PR, in both label
// namespaces, and reads nothing but the PRs themselves.
func TestList_readsTheStoredClassification(t *testing.T) {
	tools, paths := storedServer(t, []*github.PullRequest{
		{Number: new(1), Title: new("chore(deps): update x"), HTMLURL: new("https://github.com/org/one/pull/1"),
			User: &github.User{Login: new("renovate[bot]")}, Labels: []*github.Label{{Name: "marge/action-required"}}},
		{Number: new(2), Title: new("chore(deps): update y"), HTMLURL: new("https://github.com/org/one/pull/2"),
			User: &github.User{Login: new("renovate[bot]")}, Labels: []*github.Label{{Name: "bot-prs-sweep/merged"}}},
		{Number: new(3), Title: new("chore(deps): update z"), HTMLURL: new("https://github.com/org/one/pull/3"),
			User: &github.User{Login: new("dependabot[bot]")}, Labels: []*github.Label{{Name: "dependencies"}}},
	})

	result := listResult(t, tools, map[string]any{"repos": []any{"org/one"}})

	require.Len(t, result.ActionRequired, 1)
	require.Equal(t, 1, result.ActionRequired[0].Number)
	require.Equal(t, "marge/action-required", result.ActionRequired[0].Label)
	require.Equal(t, "renovate", result.ActionRequired[0].Kind)

	require.Len(t, result.Merged, 1, "a PR not swept since the rename keeps its legacy label")
	require.Equal(t, "bot-prs-sweep/merged", result.Merged[0].Label)

	require.Len(t, result.Unclassified, 1, "a PR no sweep has labelled has no stored classification")
	require.Equal(t, 3, result.Unclassified[0].Number)
	require.Empty(t, result.Unclassified[0].Label)
	require.Equal(t, 1, result.Summary.Unclassified)
	require.Equal(t, 3, result.Summary.Total)

	for _, path := range *paths {
		require.NotContains(t, path, "/pulls/", "the stored read never reads a PR: %s", path)
		require.NotContains(t, path, "check-runs", "the stored read never reads a check: %s", path)
	}
}

// TestList_refreshClassifiesAgain: the argument is what chooses between the
// two costs. A refresh reads every PR; the stored read reads none.
func TestList_refreshClassifiesAgain(t *testing.T) {
	pulls := []*github.PullRequest{
		{Number: new(1), Title: new("chore(deps): update x"), HTMLURL: new("https://github.com/org/one/pull/1"),
			User: &github.User{Login: new("renovate[bot]")}, Labels: []*github.Label{{Name: "marge/action-required"}}},
	}

	tools, paths := storedServer(t, pulls)
	listResult(t, tools, map[string]any{"repos": []any{"org/one"}, "refresh": true})
	require.Contains(t, *paths, "/repos/org/one/pulls/1", "a refresh reads the PR instead of its label")

	tools, paths = storedServer(t, pulls)
	listResult(t, tools, map[string]any{"repos": []any{"org/one"}})
	require.NotContains(t, *paths, "/repos/org/one/pulls/1")
}

// TestListTool_declaresRefresh keeps the argument and the schema in step:
// a caller cannot ask for the cheap path if the tool does not offer it.
func TestListTool_declaresRefresh(t *testing.T) {
	require.Contains(t, listTool().InputSchema.Properties, "refresh")
}

// TestListTool_describesTheDefaultItHas holds the description to what
// handleList does: the default reads the stored label, the classify step is
// what refresh: true runs, and unclassified is named as a gap in the labels
// rather than as an empty queue.
func TestListTool_describesTheDefaultItHas(t *testing.T) {
	description := listTool().Description

	require.Contains(t, description, "marge/<class> label")
	require.Contains(t, description, "unclassified means no sweep has labelled that PR")

	for _, sentence := range strings.Split(description, ". ") {
		if strings.Contains(sentence, "classify step") {
			require.Contains(t, sentence, "refresh: true",
				"the classify step is what refresh: true runs, not what the default runs")
		}
	}
}
