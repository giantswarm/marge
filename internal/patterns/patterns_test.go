package patterns

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// markerComment renders one unhandled marker as a PR comment.
func markerComment(signature string, at time.Time) *github.IssueComment {
	marker := &pr.RescueMarker{
		Tool:      "marge",
		Kind:      pr.MarkerKindEvidence,
		Outcome:   pr.MarkerOutcomeUnhandled,
		Signature: signature,
		Checks:    []string{"go-build"},
		Excerpt:   "go: updates to go.mod needed",
		HeadSHA:   "headsha",
		At:        at,
	}
	return &github.IssueComment{Body: new(marker.CommentBody())}
}

// searchServer answers the issue search with the given pull requests, and
// each pull request's comments with the markers named for it.
func searchServer(t *testing.T, markers map[int][]*github.IssueComment, queries *[]string) *github.Client {
	t.Helper()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		if queries != nil {
			*queries = append(*queries, r.URL.Query().Get("q"))
		}
		issues := make([]*github.Issue, 0, len(markers))
		for number := range markers {
			issues = append(issues, &github.Issue{
				Number:  new(number),
				HTMLURL: new(fmt.Sprintf("https://github.com/giantswarm/repo%d/pull/%d", number, number)),
			})
		}
		write(w, github.IssuesSearchResult{Total: new(len(issues)), Issues: issues})
	})
	for number, comments := range markers {
		path := fmt.Sprintf("GET /repos/giantswarm/repo%d/issues/%d/comments", number, number)
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { write(w, comments) })
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		write(w, struct{}{})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	baseURL := srv.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(srv.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return client
}

// Two PRs carrying one signature are one group of two, and the signature on
// the most PRs comes first.
func TestTopRanksSignaturesByPullRequestCount(t *testing.T) {
	now := time.Now().UTC()
	client := searchServer(t, map[int][]*github.IssueComment{
		1: {markerComment("aaaaaaaaaaaa", now)},
		2: {markerComment("aaaaaaaaaaaa", now)},
		3: {markerComment("bbbbbbbbbbbb", now)},
	}, nil)

	groups, err := Search{Client: client, Org: "giantswarm"}.Top(t.Context())
	require.NoError(t, err)
	require.Len(t, groups, 2)
	require.Equal(t, "aaaaaaaaaaaa", groups[0].Signature)
	require.Equal(t, 2, groups[0].Count)
	require.Len(t, groups[0].PRs, 2)
	require.Equal(t, []string{"go-build"}, groups[0].Checks)
	require.Contains(t, groups[0].Excerpt, "go.mod")
	require.Equal(t, 1, groups[1].Count)
}

// One signature is read back with the pull requests that carry it, which is
// what a draft is built from.
func TestSignatureReadsOneGroup(t *testing.T) {
	now := time.Now().UTC()
	var queries []string
	client := searchServer(t, map[int][]*github.IssueComment{
		1: {markerComment("aaaaaaaaaaaa", now)},
	}, &queries)

	group, err := Search{Client: client, Org: "giantswarm"}.Signature(t.Context(), "aaaaaaaaaaaa")
	require.NoError(t, err)
	require.NotNil(t, group)
	require.Equal(t, []string{"giantswarm/repo1#1"}, group.PRs)
	require.Len(t, queries, 1)
	require.Contains(t, queries[0], "org:giantswarm is:pr in:comments")
	require.Contains(t, queries[0], "aaaaaaaaaaaa")
}

// A signature no PR carries is no group, not an error: the caller says what
// to do about it.
func TestSignatureNotCarriedByAnyPullRequest(t *testing.T) {
	client := searchServer(t, map[int][]*github.IssueComment{
		1: {markerComment("aaaaaaaaaaaa", time.Now().UTC())},
	}, nil)

	group, err := Search{Client: client, Org: "giantswarm"}.Signature(t.Context(), "cccccccccccc")
	require.NoError(t, err)
	require.Nil(t, group)
}

// Since bounds the window twice: the search only returns pull requests
// updated in it, and a marker older than it is dropped even when its pull
// request was touched since.
func TestSinceDropsOlderMarkers(t *testing.T) {
	now := time.Now().UTC()
	var queries []string
	client := searchServer(t, map[int][]*github.IssueComment{
		1: {markerComment("aaaaaaaaaaaa", now.AddDate(0, 0, -30))},
		2: {markerComment("bbbbbbbbbbbb", now)},
	}, &queries)

	since := now.AddDate(0, 0, -7)
	groups, err := Search{Client: client, Org: "giantswarm", Since: since}.Top(t.Context())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "bbbbbbbbbbbb", groups[0].Signature)
	require.Contains(t, queries[0], "updated:>="+since.Format(time.DateOnly))
}

// A comment that is not an unhandled marker is not a failure: an evidence
// marker of an action that ran carries no signature and is skipped.
func TestOtherMarkersAreIgnored(t *testing.T) {
	evidence := &pr.RescueMarker{Tool: "marge", Kind: pr.MarkerKindEvidence, Outcome: "update-branch"}
	client := searchServer(t, map[int][]*github.IssueComment{
		1: {{Body: new(evidence.CommentBody())}, {Body: new("just a comment")}},
	}, nil)

	groups, err := Search{Client: client, Org: "giantswarm"}.Top(t.Context())
	require.NoError(t, err)
	require.Empty(t, groups)
}

// Known renders the signatures a search found, for the message that says a
// wanted one is not among them.
func TestKnownNamesEveryGroup(t *testing.T) {
	line := Known([]Group{{Signature: "aaaaaaaaaaaa", Count: 2, Checks: []string{"go-build"}}})
	require.True(t, strings.HasPrefix(line, "aaaaaaaaaaaa (2 PRs, go-build)"))
}
