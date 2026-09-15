package process

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/giantswarm/marge/internal/pr"
)

func repoHandler(t *testing.T, push bool, calls *atomic.Int32) *http.ServeMux {
	t.Helper()
	return repoBodyHandler(t, fmt.Sprintf(`{"name":"repo","permissions":{"admin":false,"push":%t,"pull":true}}`, push), calls)
}

func repoBodyHandler(t *testing.T, body string, calls *atomic.Int32) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	return mux
}

// noReviewsHandler answers the review list approve reads before it decides
// whether an approval is still needed.
func noReviewsHandler(mux *http.ServeMux) *http.ServeMux {
	mux.HandleFunc("GET /repos/org/repo/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})
	return mux
}

func TestEnsureWriteAccess(t *testing.T) {
	tests := []struct {
		name string
		push bool
		want error
	}{
		{"write access granted", true, nil},
		{"write access missing", false, errNoWriteAccess},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(repoHandler(t, tt.push, nil))
			defer server.Close()

			p := &Processor{Client: newTestClient(t, server)}
			err := p.ensureWriteAccess(t.Context(), "org", "repo")
			if tt.want == nil {
				if err != nil {
					t.Fatalf("ensureWriteAccess() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("ensureWriteAccess() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEnsureWriteAccess_ReadsTheRepositoryOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(repoHandler(t, true, &calls))
	defer server.Close()

	p := &Processor{Client: newTestClient(t, server)}
	for range 3 {
		if err := p.ensureWriteAccess(t.Context(), "org", "repo"); err != nil {
			t.Fatalf("ensureWriteAccess() = %v, want nil", err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("repository read %d times, want 1", got)
	}
}

func TestEnsureWriteAccess_LookupError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := &Processor{Client: newTestClient(t, server)}
	err := p.ensureWriteAccess(t.Context(), "org", "repo")
	if err == nil {
		t.Fatal("ensureWriteAccess() = nil, want an error")
	}
	if errors.Is(err, errNoWriteAccess) {
		t.Errorf("a failed lookup must not be reported as missing access: %v", err)
	}
}

// TestApprove_RefusedWithoutWriteAccess covers the whole point of the guard:
// no review is submitted when the actor's approval would not count.
func TestApprove_RefusedWithoutWriteAccess(t *testing.T) {
	var reviews atomic.Int32
	mux := noReviewsHandler(repoHandler(t, false, nil))
	mux.HandleFunc("POST /repos/org/repo/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		reviews.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := &Processor{Client: newTestClient(t, server), Login: "giantswarm-marge[bot]"}
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	err := p.approve(t.Context(), &prRun{info: info, status: status, idx: idx})
	if !errors.Is(err, errNoWriteAccess) {
		t.Fatalf("approve() = %v, want %v", err, errNoWriteAccess)
	}
	if got := reviews.Load(); got != 0 {
		t.Errorf("%d reviews submitted, want 0", got)
	}
}

func TestEnsureWriteAccess_NoPermissionsField(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no permissions block", `{"name":"repo"}`},
		{"no push field", `{"name":"repo","permissions":{"pull":true}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(repoBodyHandler(t, tt.body, nil))
			defer server.Close()

			p := &Processor{Client: newTestClient(t, server)}
			err := p.ensureWriteAccess(t.Context(), "org", "repo")
			if !errors.Is(err, errWriteAccessUnknown) {
				t.Fatalf("ensureWriteAccess() = %v, want %v", err, errWriteAccessUnknown)
			}
			if errors.Is(err, errNoWriteAccess) {
				t.Errorf("an unanswered lookup must not be reported as a denial: %v", err)
			}
		})
	}
}

// TestApprove_SettledPullRequestSkipsTheCheck keeps the guard on the path that
// submits a review. A pull request this actor already approved needs no
// review, so a repository it can no longer write must not be read or failed.
func TestApprove_SettledPullRequestSkipsTheCheck(t *testing.T) {
	var repoReads atomic.Int32
	mux := repoHandler(t, false, &repoReads)
	mux.HandleFunc("GET /repos/org/repo/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"state":"APPROVED","user":{"login":"giantswarm-marge[bot]"}}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := &Processor{Client: newTestClient(t, server), Login: "giantswarm-marge[bot]"}
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	if err := p.approve(t.Context(), &prRun{info: info, status: status, idx: idx}); err != nil {
		t.Fatalf("approve() = %v, want nil", err)
	}
	if got := repoReads.Load(); got != 0 {
		t.Errorf("repository read %d times, want 0", got)
	}
}

func TestWriteAccessDetail(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		prefix string
	}{
		{"denial", fmt.Errorf("%w: org/repo", errNoWriteAccess), "approve refused: "},
		{"unanswered", fmt.Errorf("%w: org/repo", errWriteAccessUnknown), "write access unknown: "},
		{"lookup failure", errors.New("boom"), "write access check error: "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := writeAccessDetail(tt.err)
			if !strings.HasPrefix(got, tt.prefix) {
				t.Errorf("writeAccessDetail() = %q, want prefix %q", got, tt.prefix)
			}
		})
	}
}

// TestProcessPR_DryRunReportsMissingWriteAccess covers the cheapest way an
// operator learns a permission trim broke the sweep: a dry run must name the
// refusal instead of reporting a plain skip.
func TestProcessPR_DryRunReportsMissingWriteAccess(t *testing.T) {
	mux := repoHandler(t, false, nil)
	writeJSON := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"number":1,"title":"chore(deps): update all non-major dependencies","mergeable_state":"clean","user":{"login":"renovate[bot]"},
			"head":{"sha":"aaa111","ref":"renovate/foo"},"base":{"sha":"bbb222","ref":"main"}}`)
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"state":"success","statuses":[{"context":"go-build","state":"success"}]}`)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"total_count":0,"check_runs":[]}`)
	})
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `[]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	proc := NewProcessor(newTestClient(t, server), true, false, "me")
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1, Author: "renovate[bot]"}
	idx := status.Add(info)

	proc.ProcessPR(t.Context(), info, status, idx)

	entry := status.Snapshot()[idx]
	if entry.State != pr.StatusSkipped {
		t.Fatalf("state = %v, want %v", entry.State, pr.StatusSkipped)
	}
	if !strings.Contains(entry.Detail, "dry-run") || !strings.Contains(entry.Detail, "approve refused") {
		t.Errorf("detail = %q, want it to name the dry run and the refusal", entry.Detail)
	}
}
