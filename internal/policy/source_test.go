package policy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

// githubSource serves files over the contents API and returns a source
// pointed at it, so the one type that speaks HTTP is tested against real
// protocol traffic.
func githubSource(t *testing.T, files map[string]string, status int) GitHubSource {
	t.Helper()
	const prefix = "/repos/giantswarm/github/contents/"

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix, func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			http.Error(w, `{"message":"boom"}`, status)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		kind, encoding := "file", "base64"
		if content, ok := files[path]; ok {
			body := base64.StdEncoding.EncodeToString([]byte(content))
			_ = json.NewEncoder(w).Encode(github.RepositoryContent{Type: &kind, Encoding: &encoding, Content: &body})
			return
		}
		var entries []github.RepositoryContent
		for held := range files {
			name, below := strings.CutPrefix(held, path+"/")
			if !below || strings.Contains(name, "/") {
				continue
			}
			entries = append(entries, github.RepositoryContent{Type: &kind, Name: &name, Path: &held})
		}
		if len(entries) == 0 {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return GitHubSource{Client: client, Owner: "giantswarm", Repo: "github"}
}

// TestGitHubSource_read decodes a file the repository holds.
func TestGitHubSource_read(t *testing.T) {
	source := githubSource(t, map[string]string{DefaultFile: "modelConfig: default-model-config\n"}, 0)

	require.Equal(t, "giantswarm/github", source.String())

	content, found, err := source.Read(t.Context(), DefaultFile)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "modelConfig: default-model-config\n", content)
}

// TestGitHubSource_notFoundIsNotAnError holds the rule the loader depends
// on: 404 means absent, so an absent policy file falls back to the company
// defaults instead of stopping the sweep.
func TestGitHubSource_notFoundIsNotAnError(t *testing.T) {
	source := githubSource(t, nil, 0)

	content, found, err := source.Read(t.Context(), DefaultFile)
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, content)
}

// TestGitHubSource_readFailureIsAnError separates a refusal from an absent
// file. A sweep must stop rather than run on defaults the team never wrote.
func TestGitHubSource_readFailureIsAnError(t *testing.T) {
	source := githubSource(t, nil, http.StatusInternalServerError)

	_, found, err := source.Read(t.Context(), DefaultFile)
	require.ErrorContains(t, err, "reading "+DefaultFile)
	require.False(t, found)
}
