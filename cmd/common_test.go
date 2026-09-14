package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/google/go-github/v91/github"
)

func TestParseCSVList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty string is nil", "", nil},
		{"whitespace only is nil", "   ", nil},
		{"single pattern", "trivy", []string{"trivy"}},
		{"multiple patterns", "Trivy, govulncheck ,CodeQL", []string{"Trivy", "govulncheck", "CodeQL"}},
		{"empty entries are skipped", "trivy,,gosec, ", []string{"trivy", "gosec"}},
		{"only separators yields nil", ",, ,", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCSVList(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCSVList(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseTrustedAuthors(t *testing.T) {
	got := parseTrustedAuthors("renovate[bot], dependabot[bot] , ,custom[bot]")
	want := map[string]bool{
		"renovate[bot]":   true,
		"dependabot[bot]": true,
		"custom[bot]":     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseTrustedAuthors = %v, want %v", got, want)
	}
}

// newFailingGitHubClient returns a github.Client whose every API call fails
// with HTTP 500, so each processed PR lands in the Failed bucket quickly and
// without leaving the local test server.
func newFailingGitHubClient(t *testing.T) *github.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
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

// captureFile swaps *target (os.Stdout or os.Stderr) for a pipe and returns
// a function that restores the original file and yields everything written
// in between.
func captureFile(t *testing.T, target **os.File) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := *target
	*target = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		_ = r.Close()
		done <- string(b)
	}()
	return func() string {
		_ = w.Close()
		*target = orig
		return <-done
	}
}

// TestProcessOnceWithStatus_quietWritesNothing guards the MCP stdio contract
// behind `marge serve`: stdout is the JSON-RPC transport there, so a Quiet
// run must not write a single byte to it, and it must not chatter on stderr
// either. The --no-tui path runs first as the control proving the capture
// does see the plain-text results when they are printed.
func TestProcessOnceWithStatus_quietWritesNothing(t *testing.T) {
	client := newFailingGitHubClient(t)
	prs := []pr.PRInfo{
		{Owner: "o", Repo: "r", Number: 1, Title: "Update module example.com/x to v2", Author: "renovate[bot]"},
		{Owner: "o", Repo: "r", Number: 2, Title: "Update module example.com/y to v3", Author: "renovate[bot]"},
	}

	run := func(opts RunOptions) (stdout, stderr string, status *pr.PRStatus) {
		restoreOut := captureFile(t, &os.Stdout)
		restoreErr := captureFile(t, &os.Stderr)
		status, err := processOnceWithStatus(context.Background(), client, "me", append([]pr.PRInfo(nil), prs...), opts)
		stderr = restoreErr()
		stdout = restoreOut()
		if err != nil {
			t.Fatalf("processOnceWithStatus: %v", err)
		}
		return stdout, stderr, status
	}

	// Control: --no-tui prints the plain-text results to stdout and the
	// progress line to stderr.
	stdout, stderr, status := run(RunOptions{NoTUI: true})
	if status.Len() != len(prs) {
		t.Fatalf("status.Len() = %d, want %d", status.Len(), len(prs))
	}
	if !strings.Contains(stdout, "Failed (2):") {
		t.Errorf("--no-tui stdout = %q, want the plain-text Failed group", stdout)
	}
	if !strings.Contains(stderr, "Processing 2 PR(s)") {
		t.Errorf("--no-tui stderr = %q, want the progress line", stderr)
	}

	// Quiet on its own (without NoTUI): nothing may reach stdout or stderr,
	// while the PRs are still processed.
	stdout, stderr, status = run(RunOptions{Quiet: true})
	if stdout != "" {
		t.Errorf("quiet stdout = %q, want empty", stdout)
	}
	if stderr != "" {
		t.Errorf("quiet stderr = %q, want empty", stderr)
	}
	if got := status.Summary().Failed; got != len(prs) {
		t.Errorf("quiet Failed = %d, want %d (processing must still happen)", got, len(prs))
	}
}

// TestProcessOnceWithStatus_quietNoPRs covers the early return: the
// "No matching PRs found." hint is for humans and must stay off both
// streams in quiet mode.
func TestProcessOnceWithStatus_quietNoPRs(t *testing.T) {
	restoreOut := captureFile(t, &os.Stdout)
	restoreErr := captureFile(t, &os.Stderr)
	status, err := processOnceWithStatus(context.Background(), nil, "me", nil, RunOptions{Quiet: true})
	stderr := restoreErr()
	stdout := restoreOut()
	if err != nil {
		t.Fatalf("processOnceWithStatus: %v", err)
	}
	if status.Len() != 0 {
		t.Errorf("status.Len() = %d, want 0", status.Len())
	}
	if stdout != "" || stderr != "" {
		t.Errorf("quiet run with no PRs wrote stdout=%q stderr=%q, want both empty", stdout, stderr)
	}
}
