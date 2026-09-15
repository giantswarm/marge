package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
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

// TestReadReposFile guards the repos file format shared by the --repos-file
// flags and the sweep tool's repos_file argument: one trimmed owner/name
// entry per line, blank lines and # comments ignored, and an error rather
// than an empty list when the file names no repository (an empty list
// would silently widen the run to the GitHub search).
func TestReadReposFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return path
	}

	tests := []struct {
		name    string
		path    string
		want    []string
		wantErr string
	}{
		{
			name: "entries are trimmed, comments and blank lines skipped",
			path: write("repos.txt", "# team repos\n\n  my-org/a  \nmy-org/b\n\t# trailing comment\nother/c"),
			want: []string{"my-org/a", "my-org/b", "other/c"},
		},
		{
			name: "windows line endings",
			path: write("crlf.txt", "my-org/a\r\nmy-org/b\r\n"),
			want: []string{"my-org/a", "my-org/b"},
		},
		{
			name:    "only comments is an error",
			path:    write("comments.txt", "# nothing here\n\n"),
			wantErr: "lists no repositories",
		},
		{
			name:    "missing file is an error",
			path:    filepath.Join(dir, "missing.txt"),
			wantErr: "reading repos file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readReposFile(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("readReposFile = %v, %v; want error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readReposFile: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readReposFile = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunOptions_resolveScope guards the optional flag: no --repos-file
// means no restriction (nil), a file means its entries. Neither case reads
// a team file, and a company default file that is not there leaves the
// built-in defaults in place.
func TestRunOptions_resolveScope(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", nil)

	scope, err := RunOptions{}.resolveScope(t.Context(), client)
	if err != nil || scope.Repos != nil {
		t.Errorf("RunOptions{}.resolveScope() = %v, %v; want no repositories and no error", scope.Repos, err)
	}

	path := filepath.Join(t.TempDir(), "repos.txt")
	if err := os.WriteFile(path, []byte("my-org/a\n"), 0o600); err != nil {
		t.Fatalf("writing repos file: %v", err)
	}
	scope, err = RunOptions{ReposFile: path}.resolveScope(t.Context(), client)
	if err != nil || !reflect.DeepEqual(scope.Repos, []string{"my-org/a"}) {
		t.Errorf("resolveScope() = %v, %v; want [my-org/a], nil", scope.Repos, err)
	}
}

// TestFilterByOrg guards the --org / org filter shared by run, sweep and
// the sweep tool: it matches the PR owner case-insensitively and an empty
// org keeps everything.
func TestFilterByOrg(t *testing.T) {
	prs := []pr.PRInfo{
		{Owner: "my-org", Repo: "a", Number: 1},
		{Owner: "Other", Repo: "b", Number: 2},
		{Owner: "My-Org", Repo: "c", Number: 3},
	}
	tests := []struct {
		name string
		prs  []pr.PRInfo
		org  string
		want []int
	}{
		{"empty org keeps every PR", prs, "", []int{1, 2, 3}},
		{"exact owner", prs, "Other", []int{2}},
		{"owner is matched case-insensitively", prs, "MY-ORG", []int{1, 3}},
		{"no owner matches", prs, "nobody", nil},
		{"no PRs", nil, "my-org", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []int
			for _, p := range filterByOrg(tt.prs, tt.org) {
				got = append(got, p.Number)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("filterByOrg(%q) kept %v, want %v", tt.org, got, tt.want)
			}
		})
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
