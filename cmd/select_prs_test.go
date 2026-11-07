package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// pullsServer answers GET for the PRs of giantswarm/<repo> it holds and
// counts the reads.
func pullsServer(t *testing.T, pulls map[string]*github.PullRequest) (*github.Client, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/giantswarm/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		number, _ := strconv.Atoi(r.PathValue("number"))
		pull, ok := pulls[r.PathValue("repo")+"#"+strconv.Itoa(number)]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(pull)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return client, &reads
}

func closedPull(login string, merged bool) *github.PullRequest {
	return &github.PullRequest{
		State:   new("closed"),
		Merged:  new(merged),
		Title:   new("chore: align files according to platform standards"),
		HTMLURL: new("https://github.com/giantswarm/converter/pull/20"),
		User:    &github.User{Login: new(login)},
	}
}

func TestSelectPRs(t *testing.T) {
	open := []pr.PRInfo{{Owner: "giantswarm", Repo: "converter", Number: 21, Author: "renovate[bot]"}}
	scope := []string{"giantswarm/converter"}

	t.Run("a named bot PR that merged since is gone, and the rest is kept", func(t *testing.T) {
		client, _ := pullsServer(t, map[string]*github.PullRequest{
			"converter#20": closedPull("giantswarm-align-files[bot]", true),
		})

		kept, gone, err := selectPRs(t.Context(), client, open, []string{"giantswarm/converter#20", "giantswarm/converter#21"}, scope)

		require.NoError(t, err)
		require.Equal(t, open, kept)
		require.Len(t, gone, 1)
		require.True(t, gone[0].merged)
		require.Equal(t, 20, gone[0].info.Number)
		require.Equal(t, "giantswarm-align-files[bot]", gone[0].info.Author)
	})

	t.Run("a named bot PR closed unmerged is gone too", func(t *testing.T) {
		client, _ := pullsServer(t, map[string]*github.PullRequest{
			"converter#20": closedPull("renovate[bot]", false),
		})

		_, gone, err := selectPRs(t.Context(), client, open, []string{"giantswarm/converter#20"}, scope)

		require.NoError(t, err)
		require.Len(t, gone, 1)
		require.False(t, gone[0].merged)
	})

	t.Run("a PR still open but not in the scope's list is refused", func(t *testing.T) {
		pull := closedPull("renovate[bot]", false)
		pull.State = new("open")
		client, _ := pullsServer(t, map[string]*github.PullRequest{"converter#20": pull})

		_, _, err := selectPRs(t.Context(), client, open, []string{"giantswarm/converter#20"}, scope)

		require.EqualError(t, err, "not an open bot PR of this scope: giantswarm/converter#20")
	})

	t.Run("a closed PR of a person is refused", func(t *testing.T) {
		client, _ := pullsServer(t, map[string]*github.PullRequest{
			"converter#20": closedPull("someone", true),
		})

		_, _, err := selectPRs(t.Context(), client, open, []string{"giantswarm/converter#20"}, scope)

		require.ErrorContains(t, err, "giantswarm/converter#20")
	})

	t.Run("a PR that does not exist is refused", func(t *testing.T) {
		client, _ := pullsServer(t, nil)

		_, _, err := selectPRs(t.Context(), client, open, []string{"giantswarm/converter#99"}, scope)

		require.ErrorContains(t, err, "giantswarm/converter#99")
	})

	t.Run("a PR of a repository outside the scope is refused unread", func(t *testing.T) {
		client, reads := pullsServer(t, map[string]*github.PullRequest{
			"other#20": closedPull("renovate[bot]", true),
		})

		_, _, err := selectPRs(t.Context(), client, open, []string{"giantswarm/other#20"}, scope)

		require.ErrorContains(t, err, "giantswarm/other#20")
		require.Zero(t, reads.Load())
	})

	t.Run("a selection the scope found whole reads nothing", func(t *testing.T) {
		client, reads := pullsServer(t, nil)

		kept, gone, err := selectPRs(t.Context(), client, open, []string{"giantswarm/converter#21"}, scope)

		require.NoError(t, err)
		require.Equal(t, open, kept)
		require.Empty(t, gone)
		require.Zero(t, reads.Load())
	})
}

func TestReportGone(t *testing.T) {
	status := pr.NewPRStatus()

	reportGone(status, []gonePR{
		{info: pr.PRInfo{Owner: "giantswarm", Repo: "converter", Number: 20, Author: "giantswarm-align-files[bot]"}, merged: true},
		{info: pr.PRInfo{Owner: "giantswarm", Repo: "converter", Number: 22, Author: "renovate[bot]"}},
	})

	entries := status.Snapshot()
	require.Len(t, entries, 2)
	require.Equal(t, pr.StatusAlreadyMerged, entries[0].State)
	require.Equal(t, "merged since the list was read", entries[0].Detail)
	require.Equal(t, pr.StatusSkipped, entries[1].State)
	require.Equal(t, "closed since the list was read", entries[1].Detail)
}
