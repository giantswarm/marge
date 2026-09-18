package cmd

import (
	"context"
	"encoding/base64"
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

// teamsServer serves the team files of the team-file repository, the login
// and the open-PR listing, and counts the listings: reading several teams
// must cost one.
func teamsServer(t *testing.T, files map[string]string, byRepo map[string][]*github.PullRequest) (toolset, *int) {
	t.Helper()

	var (
		mu       sync.Mutex
		listings int
	)
	pulls := graphQLPulls(t, byRepo, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(&github.User{Login: new("me")})
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		listings++
		mu.Unlock()
		pulls(w, r)
	})
	mux.HandleFunc("GET /repos/giantswarm/github/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/github/contents/")
		content, ok := files[path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		body := base64.StdEncoding.EncodeToString([]byte(content))
		_ = json.NewEncoder(w).Encode(github.RepositoryContent{Type: new("file"), Encoding: new("base64"), Content: new(body)})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return toolset{newClient: func(context.Context) (*github.Client, error) { return client, nil }}, &listings
}

func teamQueues(t *testing.T, tools toolset, arguments map[string]any) TeamQueues {
	t.Helper()
	got, err := tools.handleList(t.Context(), call(arguments))
	require.NoError(t, err)
	require.False(t, got.IsError, textOf(t, got))
	var result TeamQueues
	require.NoError(t, json.Unmarshal([]byte(textOf(t, got)), &result))
	return result
}

func botPull(repo string, number int, label string) *github.PullRequest {
	pull := &github.PullRequest{
		Number:  new(number),
		Title:   new("chore(deps): update x"),
		HTMLURL: new(fmt.Sprintf("https://github.com/giantswarm/%s/pull/%d", repo, number)),
		User:    &github.User{Login: new("renovate[bot]")},
	}
	if label != "" {
		pull.Labels = []*github.Label{{Name: label}}
	}
	return pull
}

// TestListTeams_readsEveryTeamInOneDiscovery is what the Bot PRs page buys:
// its "All teams" scope was one call per team, each resolving its own
// policy and listing its own repositories. One call now shares the listing,
// and every team still reports under its own team file.
func TestListTeams_readsEveryTeamInOneDiscovery(t *testing.T) {
	tools, listings := teamsServer(t,
		map[string]string{
			"repositories/team-bumblebee.yaml": "- name: marge\n",
			"repositories/team-atlas.yaml":     "- name: atlas\n",
		},
		map[string][]*github.PullRequest{
			"giantswarm/marge": {botPull("marge", 1, "marge/action-required")},
			"giantswarm/atlas": {botPull("atlas", 2, "marge/merged")},
		})

	result := teamQueues(t, tools, map[string]any{"teams": []any{"bumblebee", "atlas"}})

	require.Equal(t, []string{"bumblebee", "atlas"}, []string{result.Teams[0].Team, result.Teams[1].Team},
		"the teams answer in the order they were given")
	require.Equal(t, 1, *listings, "the teams share one discovery")

	require.NotNil(t, result.Teams[0].Result)
	require.Len(t, result.Teams[0].Result.ActionRequired, 1)
	require.Equal(t, 1, result.Teams[0].Result.ActionRequired[0].Number)
	require.Equal(t, 1, result.Teams[0].Result.Summary.Total, "a team reports its own PRs and no other team's")

	require.NotNil(t, result.Teams[1].Result)
	require.Len(t, result.Teams[1].Result.Merged, 1)
	require.Equal(t, 2, result.Teams[1].Result.Merged[0].Number)
}

// TestListTeams_oneTeamWithoutFilesLeavesTheOthers: a team the team-file
// repository does not know carries the refusal, and the teams next to it
// still report. The page lists every team of the catalogue, and some of them
// have no team file yet.
func TestListTeams_oneTeamWithoutFilesLeavesTheOthers(t *testing.T) {
	tools, _ := teamsServer(t,
		map[string]string{
			"repositories/team-bumblebee.yaml": "- name: marge\n",
		},
		map[string][]*github.PullRequest{
			"giantswarm/marge": {botPull("marge", 1, "marge/action-required")},
		})

	result := teamQueues(t, tools, map[string]any{"teams": []any{"nosuchteam", "bumblebee"}})

	require.Len(t, result.Teams, 2)
	require.Equal(t, "nosuchteam", result.Teams[0].Team)
	require.Nil(t, result.Teams[0].Result)
	require.NotEmpty(t, result.Teams[0].Error)
	require.NotNil(t, result.Teams[1].Result)
	require.Len(t, result.Teams[1].Result.ActionRequired, 1)
}

// TestListTeams_refusesASecondScope: the scope decides the policy, so two
// scopes in one call would leave a caller believing a team file applied to
// something it did not.
func TestListTeams_refusesASecondScope(t *testing.T) {
	tools := toolset{newClient: func(context.Context) (*github.Client, error) {
		t.Fatal("a refused call reads nothing")
		return nil, nil
	}}

	for name, arguments := range map[string]map[string]any{
		"team":  {"teams": []any{"bumblebee"}, "team": "atlas"},
		"query": {"teams": []any{"bumblebee"}, "query": "typescript"},
		"repos": {"teams": []any{"bumblebee"}, "repos": []any{"giantswarm/marge"}},
	} {
		got, err := tools.handleList(t.Context(), call(arguments))
		require.NoError(t, err, name)
		require.True(t, got.IsError, name)
		require.Contains(t, textOf(t, got), "mutually exclusive", name)
	}
}

// TestListTool_declaresTeams keeps the argument and the schema in step.
func TestListTool_declaresTeams(t *testing.T) {
	require.Contains(t, listTool().InputSchema.Properties, "teams")
}

// TestListTeams_refreshClassifiesUnderEachTeamsPolicy: the teams share the
// discovery and nothing else. A refresh reads every PR of every team, and
// each team's PRs are decided under that team's own policy.
func TestListTeams_refreshClassifiesUnderEachTeamsPolicy(t *testing.T) {
	var (
		mu   sync.Mutex
		read []string
	)
	files := map[string]string{
		"repositories/team-bumblebee.yaml": "- name: marge\n",
		"repositories/team-atlas.yaml":     "- name: atlas\n",
		// atlas merges no minor update, bumblebee takes the company
		// default, so the same PR is eligible for one team and held for
		// the other.
		"bot-prs-sweep/team-atlas.yaml": "updateTypes:\n  renovate: [patch]\n",
	}
	byRepo := map[string][]*github.PullRequest{
		"giantswarm/marge": {botPull("marge", 1, "")},
		"giantswarm/atlas": {botPull("atlas", 2, "")},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(&github.User{Login: new("me")})
	})
	mux.HandleFunc("POST /graphql", graphQLPulls(t, byRepo, nil))
	mux.HandleFunc("GET /repos/giantswarm/github/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/giantswarm/github/contents/")
		content, ok := files[path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		body := base64.StdEncoding.EncodeToString([]byte(content))
		_ = json.NewEncoder(w).Encode(github.RepositoryContent{Type: new("file"), Encoding: new("base64"), Content: new(body)})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		read = append(read, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/1"), strings.HasSuffix(r.URL.Path, "/pulls/2"):
			number := strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "")
			repo := "marge"
			if number == "2" {
				repo = "atlas"
			}
			_, _ = fmt.Fprintf(w, `{"number":%s,"title":"chore(deps): update x from 1.1.0 to 1.2.0","mergeable_state":"clean","user":{"login":"renovate[bot]"},
				"head":{"sha":"aaa","ref":"renovate/x","repo":{"full_name":"giantswarm/%s"}},"base":{"sha":"bbb","ref":"main"}}`, number, repo)
		case strings.HasSuffix(r.URL.Path, "/required_status_checks"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = io.WriteString(w, `{"state":"success","statuses":[{"context":"go-build","state":"success"}]}`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = io.WriteString(w, `{"total_count":0,"check_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `[]`)
		default:
			http.Error(w, `{"message":"unexpected `+r.URL.Path+`"}`, http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	tools := toolset{newClient: func(context.Context) (*github.Client, error) { return client, nil }}

	result := teamQueues(t, tools, map[string]any{"teams": []any{"bumblebee", "atlas"}, "refresh": true})

	require.Len(t, result.Teams, 2)
	require.NotNil(t, result.Teams[0].Result)
	require.Len(t, result.Teams[0].Result.Eligible, 1, "bumblebee takes the company default, so a minor update is eligible")
	require.NotNil(t, result.Teams[1].Result)
	require.Len(t, result.Teams[1].Result.ActionRequired, 1, "atlas merges patch updates only, so the same minor update is held for a person")

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, read, "/repos/giantswarm/marge/pulls/1", "a refresh reads every PR")
	require.Contains(t, read, "/repos/giantswarm/atlas/pulls/2")
}
