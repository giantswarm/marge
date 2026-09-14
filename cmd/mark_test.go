package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v91/github"

	"github.com/giantswarm/marge/internal/pr"
)

// markFixture fakes the three GitHub calls marge mark makes: fetch the PR,
// compare base...head for the fingerprint, post the marker comment.
type markFixture struct {
	files       []*github.CommitFile
	compareFail bool

	posted atomic.Pointer[string]
}

const markHead = "1be1ed9de7c4b7fb8bc0f4f3c07c2ce62d67e4ee"

func (f *markFixture) client(t *testing.T) *github.Client {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /repos/org/repo/pulls/44", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, github.PullRequest{
			Number:       new(44),
			Title:        new("chore(deps): update dependency typescript to v7"),
			ChangedFiles: new(len(f.files)),
			Head:         &github.PullRequestBranch{SHA: new(markHead)},
			Base:         &github.PullRequestBranch{Ref: new("main")},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/compare/main..."+markHead, func(w http.ResponseWriter, _ *http.Request) {
		if f.compareFail {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, github.CommitsComparison{Files: f.files})
	})
	mux.HandleFunc("POST /repos/org/repo/issues/44/comments", func(w http.ResponseWriter, r *http.Request) {
		var req github.IssueComment
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		f.posted.Store(req.Body)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, github.IssueComment{ID: new(int64(1)), Body: req.Body})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}
	return client
}

func TestMarkRescue_recordsFingerprint(t *testing.T) {
	patch := "@@ -20,7 +20,7 @@\n-    \"typescript\": \"^5.9.2\",\n+    \"typescript\": \"^7.0.0\","
	f := &markFixture{files: []*github.CommitFile{{
		Filename: new("package.json"), Status: new("modified"), Changes: new(2), Patch: new(patch),
	}}}

	marker, owner, repo, number, err := markRescue(context.Background(), f.client(t), "https://github.com/org/repo/pull/44", "blocked", "peer typescript <6.1.0", "klaus")
	if err != nil {
		t.Fatalf("markRescue: %v", err)
	}
	if owner != "org" || repo != "repo" || number != 44 {
		t.Errorf("target = %s/%s#%d, want org/repo#44", owner, repo, number)
	}

	wantPatchID := pr.PatchID(f.files, 1)
	if marker.HeadSHA != markHead || marker.PatchID != wantPatchID || marker.ChangeID != "typescript@v7" {
		t.Errorf("marker pins = head %q patch_id %q change_id %q; want %q %q %q", marker.HeadSHA, marker.PatchID, marker.ChangeID, markHead, wantPatchID, "typescript@v7")
	}

	posted := f.posted.Load()
	if posted == nil {
		t.Fatal("no comment was posted")
	}
	parsed := pr.ParseRescueMarker(*posted)
	if parsed == nil || parsed.Fingerprint != marker.Fingerprint || parsed.HeadSHA != markHead {
		t.Errorf("posted comment does not round-trip the fingerprint:\n%s", *posted)
	}
	if got := pinned(marker); !strings.Contains(got, "head 1be1ed9d") || !strings.Contains(got, "patch_id "+wantPatchID) || !strings.Contains(got, "change_id typescript@v7") {
		t.Errorf("pinned() = %q", got)
	}
}

func TestMarkRescue_compareFailureFallsBackToChangeID(t *testing.T) {
	f := &markFixture{compareFail: true}

	marker, _, _, _, err := markRescue(context.Background(), f.client(t), "https://github.com/org/repo/pull/44", "failed", "", "klaus")
	if err != nil {
		t.Fatalf("markRescue must not fail when the fingerprint cannot be computed: %v", err)
	}
	if marker.PatchID != "" || marker.ChangeID != "typescript@v7" {
		t.Errorf("fingerprint = %+v, want only the title-derived change id", marker.Fingerprint)
	}
	if f.posted.Load() == nil {
		t.Error("the marker comment must still be posted")
	}
}

func TestMarkRescue_rejectsUnknownOutcome(t *testing.T) {
	if _, _, _, _, err := markRescue(context.Background(), nil, "https://github.com/org/repo/pull/44", "gave-up", "", "klaus"); err == nil {
		t.Error("expected an error for an invalid outcome")
	}
}
