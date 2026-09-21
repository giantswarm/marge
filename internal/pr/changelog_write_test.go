package pr

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

func releaseNotesClient(t *testing.T, handler http.HandlerFunc) *github.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(
		github.WithHTTPClient(server.Client()),
		github.WithURLs(&baseURL, &baseURL),
	)
	require.NoError(t, err)
	return client
}

// TestReleaseNotesGenerated_theFileDecides: a cliff.toml is how a repository
// says its release notes come from its commits.
func TestReleaseNotesGenerated_theFileDecides(t *testing.T) {
	client := releaseNotesClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/org/repo/contents/cliff.toml", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","name":"cliff.toml","path":"cliff.toml"}`))
	})

	generated, err := ReleaseNotesGenerated(t.Context(), client, "org", "repo", "")

	require.NoError(t, err)
	require.True(t, generated)
}

// TestReleaseNotesGenerated_noFileIsNoError: a repository without the file
// cuts its release from the changelog, which is the case the entry is for.
func TestReleaseNotesGenerated_noFileIsNoError(t *testing.T) {
	client := releaseNotesClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	generated, err := ReleaseNotesGenerated(t.Context(), client, "org", "repo", "")

	require.NoError(t, err)
	require.False(t, generated)
}

// TestReleaseNotesGenerated_anOutageIsAnError: a read GitHub refused must
// never soften into "this repository wants an entry", which would write one
// into a repository that publishes its updates already.
func TestReleaseNotesGenerated_anOutageIsAnError(t *testing.T) {
	client := releaseNotesClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	})

	_, err := ReleaseNotesGenerated(t.Context(), client, "org", "repo", "")

	require.Error(t, err)
	require.Contains(t, err.Error(), "cliff.toml")
}

// TestWriteChangelogEntry_refusesGeneratedReleaseNotes: the refusal is on the
// write itself, so both doors -- the sweep step and the changelog tool --
// leave such a repository alone.
func TestWriteChangelogEntry_refusesGeneratedReleaseNotes(t *testing.T) {
	client := releaseNotesClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/org/repo/contents/cliff.toml", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"file","name":"cliff.toml","path":"cliff.toml"}`))
	})

	outcome, err := WriteChangelogEntry(t.Context(), client, ChangelogDefaults(), ChangelogFacts{
		Dependency: "lodash",
		From:       "4.17.20",
		To:         "4.17.21",
		PR:         "org/repo#7",
	}, "org", "repo", "renovate/lodash", false)

	require.NoError(t, err)
	require.False(t, outcome.Written)
	require.Equal(t, "org/repo generates its release notes with git-cliff", outcome.Refused)
	require.NotEmpty(t, outcome.Line)
}
