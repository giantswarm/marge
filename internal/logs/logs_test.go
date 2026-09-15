package logs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/circleci"
)

func testClient(t *testing.T, server *httptest.Server) *github.Client {
	t.Helper()
	baseURL := server.URL + "/"
	client, err := github.NewClient(
		github.WithHTTPClient(server.Client()),
		github.WithURLs(&baseURL, &baseURL),
	)
	require.NoError(t, err)
	return client
}

func TestTailKeepsTheEnd(t *testing.T) {
	got, err := tail(strings.NewReader("abcdefghij"), 4)
	require.NoError(t, err)
	require.Equal(t, "ghij", got)
}

func TestTailShorterThanTheBound(t *testing.T) {
	got, err := tail(strings.NewReader("abc"), 64)
	require.NoError(t, err)
	require.Equal(t, "abc", got)
}

func TestTailAcrossReads(t *testing.T) {
	body := strings.Repeat("x", 40<<10) + "the end"
	got, err := tail(strings.NewReader(body), 7)
	require.NoError(t, err)
	require.Equal(t, "the end", got)
}

// The Actions excerpt follows the check run's details URL to the job, then
// the redirect the API answers with.
func TestActionsReadsTheJobLog(t *testing.T) {
	logs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("step one\nError: the build failed\n"))
	}))
	defer logs.Close()

	var jobPath string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobPath = r.URL.Path
		http.Redirect(w, r, logs.URL, http.StatusFound)
	}))
	defer api.Close()

	fetcher := &Fetcher{GitHub: testClient(t, api)}
	excerpt, ok := fetcher.Actions(t.Context(), "giantswarm", "marge",
		"https://github.com/giantswarm/marge/actions/runs/99/job/4242", 1024)

	require.True(t, ok)
	require.Contains(t, excerpt, "Error: the build failed")
	require.Equal(t, "/repos/giantswarm/marge/actions/jobs/4242/logs", jobPath)
}

func TestActionsWithoutAJobInTheURL(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer api.Close()
	fetcher := &Fetcher{GitHub: testClient(t, api)}

	_, ok := fetcher.Actions(t.Context(), "giantswarm", "marge", "https://circleci.com/gh/giantswarm/marge/1", 1024)

	require.False(t, ok)
}

func TestNoClientNoExcerpt(t *testing.T) {
	var fetcher *Fetcher
	_, ok := fetcher.Actions(t.Context(), "giantswarm", "marge", "https://github.com/x/y/actions/runs/1/job/2", 64)
	require.False(t, ok)

	_, ok = (&Fetcher{}).CircleCIBuild(t.Context(), "https://circleci.com/gh/giantswarm/marge/1", 64)
	require.False(t, ok)
}

// The CircleCI excerpt holds the output of the failing steps only.
func TestCircleCIBuildReadsFailingSteps(t *testing.T) {
	const build = `{
	  "build_num": 12,
	  "status": "failed",
	  "steps": [
	    {"name": "Spin up environment", "actions": [{"name": "Spin up environment", "status": "success", "step": 0, "index": 0, "has_output": true}]},
	    {"name": "go-build", "actions": [{"name": "go-build", "status": "failed", "failed": true, "step": 4, "index": 0, "has_output": true}]}
	  ]
	}`

	var outputPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/output/") {
			outputPaths = append(outputPaths, r.URL.Path)
			_, _ = w.Write([]byte(`[{"message": "go.sum is out of date\n", "type": "out"}]`))
			return
		}
		_, _ = w.Write([]byte(build))
	}))
	defer server.Close()

	fetcher := &Fetcher{CircleCI: &circleci.Client{HTTPClient: server.Client(), BaseURL: server.URL}}
	excerpt, ok := fetcher.CircleCIBuild(t.Context(), "https://circleci.com/gh/giantswarm/marge/12", 4096)

	require.True(t, ok)
	require.Contains(t, excerpt, "go.sum is out of date")
	require.Contains(t, excerpt, "==> go-build")
	require.Equal(t, []string{"/api/v1.1/project/github/giantswarm/marge/12/output/4/0"}, outputPaths,
		"only the failing step's output is read")
}

func TestCircleCIBuildWithoutOutput(t *testing.T) {
	const build = `{"build_num": 12, "status": "failed", "steps": [
	  {"name": "go-build", "actions": [{"name": "go-build", "status": "failed", "failed": true, "step": 4, "index": 0, "has_output": false}]}
	]}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(build))
	}))
	defer server.Close()

	fetcher := &Fetcher{CircleCI: &circleci.Client{HTTPClient: server.Client(), BaseURL: server.URL}}

	_, ok := fetcher.CircleCIBuild(t.Context(), "https://circleci.com/gh/giantswarm/marge/12", 4096)

	require.False(t, ok)
}

// A job log ends with credential cleanup, so the excerpt is anchored on the
// last error the runner reported, not on the end of the file.
func TestAroundErrorAnchorsOnTheFailure(t *testing.T) {
	body := strings.Join([]string{
		"##[group]Run go build ./...",
		"internal/pr/kind.go:88:9: cannot use kind as pr.Kind value",
		"##[error]Process completed with exit code 1.",
		"Post job cleanup.",
		"[command]/usr/bin/git config --local --unset includeif.gitdir",
		"Removing credentials config",
		"Cleaning up orphan processes",
	}, "\n")

	excerpt := aroundError(body, 200)

	require.Contains(t, excerpt, "cannot use kind as pr.Kind value")
	require.NotContains(t, excerpt, "Cleaning up orphan processes")
}

func TestAroundErrorWithoutAMarker(t *testing.T) {
	require.Equal(t, "cdef", aroundError("abcdef", 4))
}

func TestAroundErrorShorterThanTheBound(t *testing.T) {
	body := "one\n##[error]two\ntrailer\n"
	require.Equal(t, "one\n##[error]two\n", aroundError(body, 4096))
}

func TestPlainText(t *testing.T) {
	raw := "2026-09-15T10:11:02.6059740Z \x1b[90m│\x1b[39m \x1b[38;2;241;97;97mTypeError: boom\x1b[39m\r\n" +
		"2026-09-15T10:11:02.6060980Z   at resolveTypescriptProject\n"

	got := PlainText(raw)

	require.Equal(t, "│ TypeError: boom\n  at resolveTypescriptProject\n", got)
}

// Two occurrences of one failure differ only in their timestamps and their
// colouring, so the stripped text is what identifies the failure.
func TestPlainTextIsStableAcrossRuns(t *testing.T) {
	first := "2026-09-15T10:11:02.6059740Z \x1b[31mError: boom\x1b[0m\n"
	second := "2026-09-16T22:04:51.1000000Z Error: boom\n"

	require.Equal(t, PlainText(first), PlainText(second))
}
