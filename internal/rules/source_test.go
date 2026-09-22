package rules

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

// catalogueServer serves a rule directory the way GitHub does: the contents
// API lists the directory, and each document is read as a git blob. The one
// loader that speaks HTTP is tested against that traffic, so the local
// directory the rest of the tests use cannot hide a mistake in it.
func catalogueServer(t *testing.T, docs map[string]string, encoding string) *github.Client {
	t.Helper()

	shaOf := func(name string) string { return "sha-" + name }
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/giantswarm/marge/contents/rules", func(w http.ResponseWriter, r *http.Request) {
		if ref := r.URL.Query().Get("ref"); ref != "main" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		entries := make([]github.RepositoryContent, 0, len(docs))
		for name := range docs {
			entries = append(entries, github.RepositoryContent{
				Type: new("file"), Name: new(name), Path: new("rules/" + name), SHA: new(shaOf(name)),
			})
		}
		write(w, entries)
	})
	mux.HandleFunc("GET /repos/giantswarm/marge/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		for name, body := range docs {
			if shaOf(name) != r.PathValue("sha") {
				continue
			}
			content := body
			if encoding == "base64" {
				content = base64.StdEncoding.EncodeToString([]byte(body))
			}
			write(w, github.Blob{SHA: new(shaOf(name)), Content: new(content), Encoding: new(encoding)})
			return
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return client
}

// TestLoadReadsTheCatalogueFromARepository is the sweep's own path: every
// run reads the catalogue from the default branch of giantswarm/marge, and
// no other test covers the requests that takes.
func TestLoadReadsTheCatalogueFromARepository(t *testing.T) {
	for _, encoding := range []string{"base64", "utf-8"} {
		t.Run(encoding, func(t *testing.T) {
			client := catalogueServer(t, map[string]string{
				"close-obsolete.yaml": ruleDoc("close-obsolete", "close"),
				"update-behind.yaml":  ruleDoc("update-behind", "update-branch"),
				"notes.md":            "not a rule",
			}, encoding)

			cat, err := Loader{Client: client}.Load(t.Context(), testRegistry())
			require.NoError(t, err)

			require.Empty(t, cat.Skipped)
			names := make([]string, len(cat.Rules))
			for i, r := range cat.Rules {
				names[i] = r.Name
			}
			require.Equal(t, []string{"close-obsolete", "update-behind"}, names)
			require.Equal(t, "giantswarm/marge@main:rules", cat.Source)
			require.Equal(t, DefaultRef, cat.Ref)
			require.NotEmpty(t, cat.Digest)
		})
	}
}

// The digest identifies the exact catalogue a sweep ran, and the evidence of
// every action carries it. It is built from the blob SHAs, so a changed
// document changes it.
func TestRepositoryDigestFollowsTheBlobSHAs(t *testing.T) {
	load := func(docs map[string]string) string {
		cat, err := Loader{Client: catalogueServer(t, docs, "base64")}.Load(t.Context(), testRegistry())
		require.NoError(t, err)
		return cat.Digest
	}

	one := load(map[string]string{"close-obsolete.yaml": ruleDoc("close-obsolete", "close")})
	same := load(map[string]string{"close-obsolete.yaml": ruleDoc("close-obsolete", "close")})
	other := load(map[string]string{"update-behind.yaml": ruleDoc("update-behind", "update-branch")})

	require.Equal(t, one, same)
	require.NotEqual(t, one, other)
}

// A repository with no rule directory is an error, not an empty catalogue:
// the sweep runs without remedies rather than deciding that no rule matches.
func TestLoadReportsAMissingRuleDirectory(t *testing.T) {
	client := catalogueServer(t, map[string]string{"close-obsolete.yaml": ruleDoc("close-obsolete", "close")}, "base64")

	_, err := Loader{Client: client, Ref: "no-such-branch"}.Load(t.Context(), testRegistry())

	require.ErrorContains(t, err, "no rule directory at giantswarm/marge@no-such-branch:rules")
}
