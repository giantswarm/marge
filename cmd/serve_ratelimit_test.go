package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/require"

	gh "github.com/giantswarm/marge/internal/github"
)

// spentToken answers what GitHub answers a token whose primary limit is
// spent.
func spentToken(w http.ResponseWriter, reset time.Time) {
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for user ID 1."}`)
}

// rateServer serves one team, marge, with the PRs given, behind the
// process's own transport, so a refusal reaches the gate the way it does in
// production. route answers every other request.
func rateServer(t *testing.T, pulls []*github.PullRequest, route http.HandlerFunc) toolset {
	t.Helper()
	tools, _ := teamsServer(t,
		map[string]string{"repositories/team-bumblebee.yaml": "- name: marge\n"},
		map[string][]*github.PullRequest{"giantswarm/marge": pulls},
		func(mux *http.ServeMux) { mux.HandleFunc("/", route) })
	served, err := tools.newClient(t.Context())
	require.NoError(t, err)
	baseURL := served.BaseURL()
	client, err := github.NewClient(github.WithTransport(gh.SharedTransport()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return toolset{newClient: func(context.Context) (*github.Client, error) { return client, nil }}
}

// greenPull answers the PR read of a green minor update.
func greenPull(w http.ResponseWriter, number string) {
	_, _ = fmt.Fprintf(w, `{"number":%s,"title":"chore(deps): update x from 1.1.0 to 1.2.0","mergeable_state":"clean","user":{"login":"renovate[bot]"},
		"head":{"sha":"aaa","ref":"renovate/x","repo":{"full_name":"giantswarm/marge"}},"base":{"sha":"bbb","ref":"main"}}`, number)
}

// A sweep that meets a spent token stops, and what it had not decided keeps
// the label it had. A check read refused for rate is no evidence: deciding
// on it would label a green PR action_required.
func TestSweep_stopsOnASpentTokenAndLabelsNothingItDidNotDecide(t *testing.T) {
	reset := time.Now().Add(40 * time.Minute).Truncate(time.Second)
	var (
		mu      sync.Mutex
		labels  []string
		spent   atomic.Bool
		checked atomic.Int32
	)
	tools := rateServer(t,
		[]*github.PullRequest{botPull("marge", 1, "marge/eligible"), botPull("marge", 2, "marge/eligible")},
		func(w http.ResponseWriter, r *http.Request) {
			if spent.Load() {
				spentToken(w, reset)
				return
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/labels"):
				mu.Lock()
				labels = append(labels, r.URL.Path)
				mu.Unlock()
				_, _ = io.WriteString(w, `[]`)
			case strings.HasSuffix(r.URL.Path, "/pulls/1"), strings.HasSuffix(r.URL.Path, "/pulls/2"):
				greenPull(w, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			case strings.HasSuffix(r.URL.Path, "/status"):
				// The token runs dry on the first check read.
				checked.Add(1)
				spent.Store(true)
				spentToken(w, reset)
			default:
				http.NotFound(w, r)
			}
		})

	got, err := tools.handleSweep(t.Context(), call(map[string]any{"team": "bumblebee", "actions": "classify"}))
	require.NoError(t, err)
	require.False(t, got.IsError, textOf(t, got))

	var result SweepResult
	require.NoError(t, json.Unmarshal([]byte(textOf(t, got)), &result))
	require.NotNil(t, result.RateLimit, "the result says the run stopped on the rate limit")
	require.Equal(t, reset.UTC().Format(time.RFC3339), result.RateLimit.ResetAt)
	require.Empty(t, result.ActionRequired, "a refused check read is not a failing check")
	require.Equal(t, 2, result.Summary.NotReached)
	require.Len(t, result.NotReached, 2)

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, labels, "no PR is labelled from a run that stopped before deciding it")
}

// A run the token cannot cover is refused before its first PR read. Two PRs
// of one repository need seven requests at the least.
func TestSweep_refusesARunTheBudgetCannotCover(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	var pullReads atomic.Int32
	tools := rateServer(t,
		[]*github.PullRequest{botPull("marge", 1, ""), botPull("marge", 2, "")},
		func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/rate_limit":
				_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{
					"core": map[string]any{"limit": 5000, "remaining": 2, "reset": reset.Unix()},
				}})
			case strings.Contains(r.URL.Path, "/pulls/"):
				pullReads.Add(1)
				greenPull(w, "1")
			default:
				http.NotFound(w, r)
			}
		})

	got, err := tools.handleSweep(t.Context(), call(map[string]any{"team": "bumblebee", "dry_run": true}))
	require.NoError(t, err)
	require.True(t, got.IsError)
	require.Equal(t,
		"GitHub rate limit too low for this run: 2 requests left of the 7 it needs at the least; it resets at "+reset.UTC().Format(time.RFC3339),
		textOf(t, got))
	require.Zero(t, pullReads.Load(), "no PR is read for a run that cannot finish")
}

// GET /user is the first request of every call, so it is where a spent
// token is met first. Every team of the call reads the same refusal, with
// the time the token is served again, and not the raw GitHub error.
func TestListTeams_aSpentTokenReadsAsTheRateLimit(t *testing.T) {
	reset := time.Now().Add(31 * time.Minute).Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) { spentToken(w, reset) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	tools := toolset{newClient: func(context.Context) (*github.Client, error) { return client, nil }}

	got, err := tools.handleList(t.Context(), call(map[string]any{"teams": []any{"bumblebee", "atlas"}}))
	require.NoError(t, err)
	require.True(t, got.IsError)
	require.Equal(t, "GitHub rate limit reached for this token; it resets at "+reset.UTC().Format(time.RFC3339), textOf(t, got))
}
