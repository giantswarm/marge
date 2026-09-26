package process

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// An upstream sync PR is sized by its diff: giantswarm/kagent#80 moves every
// vendored chart from 0.10.1 to 0.10.2, a patch, while its title and body
// name only the target version.
func TestClassifyChartSync_readsTheSizeOffTheComparison(t *testing.T) {
	sync := "@@ -1,2 +1,2 @@\n-appVersion: 0.10.1\n+appVersion: 0.10.2\n"
	for name, tc := range map[string]struct {
		files int
		patch string
		want  pr.UpdateType
	}{
		"a patch sync":                    {files: 1, patch: sync, want: pr.UpdatePatch},
		"a major sync":                    {files: 1, patch: "@@ -1 +1 @@\n-version: 0.10.1\n+version: 1.0.0\n", want: pr.UpdateMajor},
		"a comparison truncated at limit": {files: compareFileLimit, patch: sync, want: pr.UpdateUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /repos/org/kagent/compare/main...head", func(w http.ResponseWriter, r *http.Request) {
				files := make([]*github.CommitFile, 0, tc.files)
				for i := range tc.files {
					files = append(files, &github.CommitFile{Filename: new(fmt.Sprintf("helm/c%d/Chart.yaml", i)), Patch: new(tc.patch)})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(github.CommitsComparison{Files: files})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			proc := NewProcessor(newTestClient(t, srv), true, false, "me")
			pull := &github.PullRequest{
				Head: &github.PullRequestBranch{SHA: new("head"), Ref: new("update-chart")},
				Base: &github.PullRequestBranch{Ref: new("main")},
			}
			got := proc.classifyChartSync(t.Context(), pr.PRInfo{Owner: "org", Repo: "kagent", Number: 80}, pull)
			require.Equal(t, tc.want, got)
		})
	}
}

// A comparison that cannot be had leaves the size unread, never guessed.
func TestClassifyChartSync_unknownWithoutAComparison(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	proc := NewProcessor(newTestClient(t, srv), true, false, "me")
	pull := &github.PullRequest{
		Head: &github.PullRequestBranch{SHA: new("head")},
		Base: &github.PullRequestBranch{Ref: new("main")},
	}
	require.Equal(t, pr.UpdateUnknown, proc.classifyChartSync(t.Context(), pr.PRInfo{Owner: "org", Repo: "kagent", Number: 80}, pull))
}
