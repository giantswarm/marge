package process

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/giantswarm/marge/internal/pr"
)

func repoHandler(t *testing.T, push bool, calls *atomic.Int32) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"repo","permissions":{"admin":false,"push":%t,"pull":true}}`, push)
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
	mux := repoHandler(t, false, nil)
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

	err := p.approve(t.Context(), info, status, idx)
	if !errors.Is(err, errNoWriteAccess) {
		t.Fatalf("approve() = %v, want %v", err, errNoWriteAccess)
	}
	if got := reviews.Load(); got != 0 {
		t.Errorf("%d reviews submitted, want 0", got)
	}
}
