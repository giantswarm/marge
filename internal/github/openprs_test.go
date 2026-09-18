package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

// graphQLServer answers every GraphQL request with answer, and records the
// requests it was sent.
type graphQLServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
}

func newGraphQLServer(t *testing.T, answer func(variables map[string]any) string) *graphQLServer {
	t.Helper()
	server := &graphQLServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/graphql", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(body, &request))
		server.mu.Lock()
		server.requests = append(server.requests, map[string]any{"query": request.Query, "variables": request.Variables})
		server.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer(request.Variables))
	}))
	t.Cleanup(server.Close)
	return server
}

func graphQLClient(t *testing.T, server *graphQLServer) *github.Client {
	t.Helper()
	base := server.URL + "/"
	client, err := github.NewClient(github.WithURLs(&base, &base))
	require.NoError(t, err)
	return client
}

func prNode(number int, login, typeName string, labels ...string) string {
	quoted := make([]string, 0, len(labels))
	for _, label := range labels {
		quoted = append(quoted, fmt.Sprintf(`{"name":%q}`, label))
	}
	return fmt.Sprintf(`{"number":%d,"title":"chore: bump","url":"https://github.com/giantswarm/a/pull/%d",
		"createdAt":"2026-09-01T10:00:00Z","baseRefName":"main",
		"author":{"login":%q,"__typename":%q},"labels":{"nodes":[%s]}}`,
		number, number, login, typeName, strings.Join(quoted, ","))
}

// TestListOpenPRs reads several repositories in one request, and restores
// the [bot] suffix REST reports and GraphQL leaves off.
func TestListOpenPRs(t *testing.T) {
	server := newGraphQLServer(t, func(map[string]any) string {
		return fmt.Sprintf(`{"data":{
			"r0":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}},
			"r1":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}}
		}}`,
			prNode(1, "renovate", "Bot", "marge/eligible"),
			prNode(2, "marians", "User"))
	})

	prs, failures := ListOpenPRs(t.Context(), graphQLClient(t, server), ParseRepoRefs([]string{"giantswarm/a", "giantswarm/b"}))

	require.Empty(t, failures)
	require.Len(t, server.requests, 1)
	require.Len(t, prs, 2)
	require.Equal(t, "renovate[bot]", prs[0].Author)
	require.Equal(t, []string{"marge/eligible"}, prs[0].Labels)
	require.Equal(t, "giantswarm", prs[0].Owner)
	require.Equal(t, "a", prs[0].Repo)
	require.Equal(t, "main", prs[0].BaseRef)
	require.Equal(t, 2026, prs[0].CreatedAt.Year())
	require.Equal(t, "marians", prs[1].Author)
	require.Equal(t, "b", prs[1].Repo)
	require.Nil(t, prs[1].Labels)

	// The names travel as variables, so a repository name is never part of
	// the query text.
	query := server.requests[0]["query"].(string)
	require.NotContains(t, query, "giantswarm/a")
	require.Equal(t, "giantswarm", server.requests[0]["variables"].(map[string]any)["o0"])
	require.Equal(t, "a", server.requests[0]["variables"].(map[string]any)["n0"])
}

// TestListOpenPRsReportsUnreadableRepo: GitHub answers 200 with the fields it
// could resolve and an error per field it could not, so one unreadable
// repository must not cost the others their pull requests.
func TestListOpenPRsReportsUnreadableRepo(t *testing.T) {
	server := newGraphQLServer(t, func(map[string]any) string {
		return fmt.Sprintf(`{"data":{"r0":null,"r1":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}},
			"errors":[{"type":"NOT_FOUND","path":["r0"],"message":"Could not resolve to a Repository with the name 'giantswarm/gone'."}]}`,
			prNode(2, "renovate", "Bot"))
	})

	prs, failures := ListOpenPRs(t.Context(), graphQLClient(t, server), ParseRepoRefs([]string{"giantswarm/gone", "giantswarm/b"}))

	require.Len(t, prs, 1)
	require.Equal(t, 2, prs[0].Number)
	require.Len(t, failures, 1)
	require.Equal(t, "giantswarm/gone", failures[0].Repo)
	require.Contains(t, failures[0].Err, "Could not resolve")
}

// TestListOpenPRsPagesOneRepo: a repository with more open pull requests than
// one page is read on for that repository alone.
func TestListOpenPRsPagesOneRepo(t *testing.T) {
	server := newGraphQLServer(t, func(variables map[string]any) string {
		if variables["after"] == nil {
			return fmt.Sprintf(`{"data":{"r0":{"pullRequests":{"pageInfo":{"hasNextPage":true,"endCursor":"CUR"},"nodes":[%s]}}}}`,
				prNode(1, "renovate", "Bot"))
		}
		require.Equal(t, "CUR", variables["after"])
		return fmt.Sprintf(`{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}}}`,
			prNode(2, "dependabot", "Bot"))
	})

	prs, failures := ListOpenPRs(t.Context(), graphQLClient(t, server), ParseRepoRefs([]string{"giantswarm/a"}))

	require.Empty(t, failures)
	require.Len(t, prs, 2)
	require.Equal(t, []int{1, 2}, []int{prs[0].Number, prs[1].Number})
	require.Len(t, server.requests, 2)
}

// TestListOpenPRsRequestFailure: a request that never answered is every
// repository of its chunk failing, so none of them is silently dropped.
func TestListOpenPRsRequestFailure(t *testing.T) {
	server := newGraphQLServer(t, func(map[string]any) string { return "" })
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	})

	prs, failures := ListOpenPRs(t.Context(), graphQLClient(t, server), ParseRepoRefs([]string{"giantswarm/a", "giantswarm/b"}))

	require.Empty(t, prs)
	require.Len(t, failures, 2)
	require.Contains(t, failures[0].Err, "401")
}

func TestParseRepoRefs(t *testing.T) {
	refs := ParseRepoRefs([]string{" giantswarm/a ", "no-slash", "giantswarm/", "/b", "", "giantswarm/c"})

	require.Equal(t, []RepoRef{{"giantswarm", "a"}, {"giantswarm", "c"}}, refs)
}

func TestGraphQLURL(t *testing.T) {
	for name, want := range map[string]string{
		"https://api.github.com/":          "https://api.github.com/graphql",
		"https://ghes.example.com/api/v3/": "https://ghes.example.com/api/graphql",
	} {
		base := name
		client, err := github.NewClient(github.WithURLs(&base, &base))
		require.NoError(t, err)
		got, err := GraphQLURL(client)
		require.NoError(t, err)
		require.Equal(t, want, got, name)
	}
}
