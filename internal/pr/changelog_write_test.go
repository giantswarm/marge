package pr

import (
	"encoding/base64"
	"encoding/json"
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

// TestWriteChangelogEntry_theAppAuthorsTheCommit is giantswarm/marge#199: a
// sweep run with a person's token wrote the entry as that person, and
// Renovate stopped rebasing every branch it touched. The author is the App,
// whatever the token; the committer is left to the token.
func TestWriteChangelogEntry_theAppAuthorsTheCommit(t *testing.T) {
	var written github.RepositoryContentFileOptions
	client := releaseNotesClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/org/repo/contents/cliff.toml":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/contents/CHANGELOG.md":
			content := base64.StdEncoding.EncodeToString([]byte("# Changelog\n\n## [Unreleased]\n\n## [1.0.0] - 2026-01-01\n"))
			_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","sha":"abc","content":"` + content + `"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/org/repo/contents/CHANGELOG.md":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&written))
			_, _ = w.Write([]byte(`{"commit":{"sha":"def"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	outcome, err := WriteChangelogEntry(t.Context(), client, ChangelogDefaults(), ChangelogFacts{
		Dependency: "lodash",
		From:       "4.17.20",
		To:         "4.17.21",
		PR:         "org/repo#7",
	}, "org", "repo", "renovate/lodash", false)

	require.NoError(t, err)
	require.True(t, outcome.Written, outcome.Refused)
	require.NotNil(t, written.Author)
	require.Equal(t, "giantswarm-marge[bot]", written.Author.GetName())
	require.Equal(t, "329428426+giantswarm-marge[bot]@users.noreply.github.com", written.Author.GetEmail())
	require.Nil(t, written.Committer, "the committer stays the token's identity")
}

// TestBotRebasesAfter: the PR's own bot and marge keep the branch the bot's;
// anyone else, linked or only an address, takes it over.
func TestBotRebasesAfter(t *testing.T) {
	require.True(t, BotRebasesAfter("renovate[bot]", "renovate[bot]", ""))
	require.True(t, BotRebasesAfter("renovate[bot]", AppLogin, ""))
	require.True(t, BotRebasesAfter("renovate[bot]", "", AppEmail))
	require.False(t, BotRebasesAfter("renovate[bot]", "teemow", "timo@example.com"))
	require.False(t, BotRebasesAfter("renovate[bot]", "", "someone@example.com"))
}
