package process

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

func TestIsBaseBranchModified(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"generic error", fmt.Errorf("something went wrong"), false},
		{"409 conflict", fmt.Errorf("409 conflict"), false},
		{"base branch modified", fmt.Errorf("Base branch was modified. Review and try the merge again."), true},
		{"base branch modified lowercase", fmt.Errorf("base branch was modified"), true},
		{"required status check", fmt.Errorf("Required status check \"build\" is expected"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBaseBranchModified(tt.err)
			if got != tt.want {
				t.Errorf("isBaseBranchModified(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// newTestClient creates a github.Client pointing at the given httptest.Server.
func newTestClient(t *testing.T, server *httptest.Server) *github.Client {
	t.Helper()
	baseURL := server.URL + "/"
	client, err := github.NewClient(
		github.WithHTTPClient(server.Client()),
		github.WithURLs(&baseURL, &baseURL),
	)
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}
	return client
}

func TestMerge_SuccessOnFirstAttempt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/org/repo/pulls/1/merge", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(github.PullRequestMergeResult{
			Merged: new(true),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server)
	proc := NewProcessor(client, false, false, "testuser", DefaultTrustedAuthors)

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	proc.merge(context.Background(), info, status, idx)

	snap := status.Snapshot()
	if snap[idx].State != pr.StatusMerged {
		t.Errorf("state = %v, want StatusMerged", snap[idx].State)
	}
}

func TestMerge_RetriesOnBaseBranchModified(t *testing.T) {
	var mu sync.Mutex
	mergeAttempts := 0

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/org/repo/pulls/1/merge", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mergeAttempts++
		attempt := mergeAttempts
		mu.Unlock()

		if attempt < 3 {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"message": "Base branch was modified. Review and try the merge again.",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(github.PullRequestMergeResult{
			Merged: new(true),
		})
	})
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(github.PullRequest{
			Number:         new(1),
			Merged:         new(false),
			MergeableState: new("clean"),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server)
	proc := &Processor{
		Client:         client,
		DryRun:         false,
		MergeAutoMerge: false,
		Login:          "testuser",
		TrustedAuthors: DefaultTrustedAuthors,
		MergeRetryWait: 1 * time.Millisecond,
	}

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	proc.merge(context.Background(), info, status, idx)

	mu.Lock()
	attempts := mergeAttempts
	mu.Unlock()

	if attempts != 3 {
		t.Errorf("merge attempts = %d, want 3", attempts)
	}

	snap := status.Snapshot()
	if snap[idx].State != pr.StatusMerged {
		t.Errorf("state = %v, want StatusMerged", snap[idx].State)
	}
}

func TestMerge_PermanentErrorNoRetry(t *testing.T) {
	var mu sync.Mutex
	mergeAttempts := 0

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/org/repo/pulls/1/merge", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mergeAttempts++
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "409 Conflict",
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server)
	proc := NewProcessor(client, false, false, "testuser", DefaultTrustedAuthors)

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	proc.merge(context.Background(), info, status, idx)

	mu.Lock()
	attempts := mergeAttempts
	mu.Unlock()
	if attempts != 1 {
		t.Errorf("merge attempts = %d, want 1 (no retries for permanent errors)", attempts)
	}

	snap := status.Snapshot()
	if snap[idx].State != pr.StatusConflict {
		t.Errorf("state = %v, want StatusConflict", snap[idx].State)
	}
}

func TestMerge_ExhaustsRetries(t *testing.T) {
	var mu sync.Mutex
	mergeAttempts := 0

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/org/repo/pulls/1/merge", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mergeAttempts++
		mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "Base branch was modified. Review and try the merge again.",
		})
	})
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(github.PullRequest{
			Number:         new(1),
			Merged:         new(false),
			MergeableState: new("clean"),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server)
	proc := &Processor{
		Client:         client,
		DryRun:         false,
		MergeAutoMerge: false,
		Login:          "testuser",
		TrustedAuthors: DefaultTrustedAuthors,
		MergeRetryWait: 1 * time.Millisecond,
	}

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	proc.merge(context.Background(), info, status, idx)

	mu.Lock()
	attempts := mergeAttempts
	mu.Unlock()
	if attempts != mergeMaxRetries {
		t.Errorf("merge attempts = %d, want %d", attempts, mergeMaxRetries)
	}

	snap := status.Snapshot()
	if snap[idx].State != pr.StatusFailed {
		t.Errorf("state = %v, want StatusFailed", snap[idx].State)
	}
	if !strings.Contains(snap[idx].Detail, "base branch modified") {
		t.Errorf("detail = %q, want it to contain 'base branch modified'", snap[idx].Detail)
	}
}

func TestMerge_MergedBetweenRetries(t *testing.T) {
	var mu sync.Mutex
	mergeAttempts := 0

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/org/repo/pulls/1/merge", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mergeAttempts++
		mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "Base branch was modified. Review and try the merge again.",
		})
	})
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		// PR was merged by someone else between retries
		_ = json.NewEncoder(w).Encode(github.PullRequest{
			Number: new(1),
			Merged: new(true),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server)
	proc := &Processor{
		Client:         client,
		DryRun:         false,
		MergeAutoMerge: false,
		Login:          "testuser",
		TrustedAuthors: DefaultTrustedAuthors,
		MergeRetryWait: 1 * time.Millisecond,
	}

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	proc.merge(context.Background(), info, status, idx)

	mu.Lock()
	attempts := mergeAttempts
	mu.Unlock()
	if attempts != 1 {
		t.Errorf("merge attempts = %d, want 1 (should stop after finding PR merged)", attempts)
	}

	snap := status.Snapshot()
	if snap[idx].State != pr.StatusAlreadyMerged {
		t.Errorf("state = %v, want StatusAlreadyMerged", snap[idx].State)
	}
}

func TestMerge_CancelledDuringRetry(t *testing.T) {
	// This test verifies that a cancelled context during the retry backoff
	// wait causes the merge to exit with StatusSkipped. We use a very short
	// retry wait and cancel the context from a goroutine after the first
	// merge attempt completes.
	var mu sync.Mutex
	mergeAttempts := 0
	firstAttemptDone := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/org/repo/pulls/1/merge", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mergeAttempts++
		mu.Unlock()

		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "Base branch was modified. Review and try the merge again.",
		})
	})
	// Serve a GET for the re-fetch that happens after the wait if the
	// cancel races and the wait completes first.
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(github.PullRequest{
			Number:         new(1),
			Merged:         new(false),
			MergeableState: new("clean"),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server)
	proc := &Processor{
		Client:         client,
		DryRun:         false,
		MergeAutoMerge: false,
		Login:          "testuser",
		TrustedAuthors: DefaultTrustedAuthors,
		MergeRetryWait: 5 * time.Second,
	}

	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 1}
	idx := status.Add(info)

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context shortly after the first merge attempt.
	go func() {
		// Wait for the merge handler to be called at least once.
		for {
			mu.Lock()
			n := mergeAttempts
			mu.Unlock()
			if n >= 1 {
				close(firstAttemptDone)
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
		// Give the merge function time to process the error and enter the select.
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	proc.merge(ctx, info, status, idx)
	<-firstAttemptDone

	snap := status.Snapshot()
	if snap[idx].State != pr.StatusSkipped {
		t.Errorf("state = %v, want StatusSkipped", snap[idx].State)
	}
}
