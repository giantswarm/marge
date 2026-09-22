package process

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

const concurrentBase = "concurrentbase00000000000000000000000"

// countingServer answers what a classification asks about any PR of one
// repository, and counts the requests it received per path.
type countingServer struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *countingServer) record(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[path]++
}

func (c *countingServer) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[path]
}

// concurrentServer serves prCount failing PRs of one repository, each with
// its own head and the base head they share.
func concurrentServer(t *testing.T, prCount int) (*countingServer, *httptest.Server) {
	t.Helper()
	counted := &countingServer{counts: make(map[string]int)}

	write := func(w http.ResponseWriter, path string, v any) {
		counted.record(path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	head := func(number int) string { return fmt.Sprintf("head%033d", number) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, _ := strconv.Atoi(r.PathValue("number"))
		write(w, "pull", github.PullRequest{
			Number:         new(number),
			State:          new("open"),
			Title:          new("chore(deps): update module golang.org/x/net to v0.46.0"),
			MergeableState: new("clean"),
			User:           &github.User{Login: new("renovate[bot]")},
			Head: &github.PullRequestBranch{
				SHA:  new(head(number)),
				Ref:  new(fmt.Sprintf("renovate/x-net-%d", number)),
				Repo: &github.Repository{FullName: new("org/repo")},
			},
			Base: &github.PullRequestBranch{SHA: new(concurrentBase), Ref: new("main")},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/{number}/head/status", func(w http.ResponseWriter, r *http.Request) {
		number, _ := strconv.Atoi(r.PathValue("number"))
		write(w, "head-status", github.CombinedStatus{State: new("failure"), SHA: new(head(number))})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/{number}/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		write(w, "head-check-runs", github.ListCheckRunsResults{Total: new(1), CheckRuns: []*github.CheckRun{{
			ID: new(int64(9)), Name: new("go-build"),
			Status: new("completed"), Conclusion: new("failure"),
			DetailsURL: new("https://github.com/org/repo/actions/runs/7/job/9"),
		}}})
	})
	mux.HandleFunc("GET /repos/org/repo/compare/{comparison}", func(w http.ResponseWriter, _ *http.Request) {
		write(w, "compare", github.CommitsComparison{
			Status: new("ahead"), BehindBy: new(0), AheadBy: new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(concurrentBase)},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		counted.record("required-status-checks")
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_pull_request_reviews", func(w http.ResponseWriter, r *http.Request) {
		counted.record("required-reviews")
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+concurrentBase+"/status", func(w http.ResponseWriter, _ *http.Request) {
		write(w, "base-status", github.CombinedStatus{State: new("success"), SHA: new(concurrentBase)})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+concurrentBase+"/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		write(w, "base-check-runs", github.ListCheckRunsResults{Total: new(0)})
	})
	mux.HandleFunc("GET /repos/org/repo/issues/{number}/comments", func(w http.ResponseWriter, _ *http.Request) {
		write(w, "comments", []*github.IssueComment{})
	})
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		write(w, "repository", github.Repository{
			FullName:    new("org/repo"),
			Permissions: &github.RepositoryPermissions{Push: new(true)},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		write(w, "other:"+r.URL.Path, struct{}{})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return counted, server
}

// The PRs of a sweep run together on one Processor: a policy may put twenty
// of them in flight, and the read path puts sixty. Every cache the
// classification reads through is therefore shared mutable state. This runs
// the whole classification of twenty PRs at once through one Processor, so
// -race sees the caches, and asserts that each cached read reached GitHub
// once for the key the PRs share rather than once per PR.
func TestProcessPRSharesItsCachesUnderConcurrency(t *testing.T) {
	const prCount = 20
	counted, server := concurrentServer(t, prCount)

	proc := NewProcessor(newTestClient(t, server), true, false, "marge")
	proc.Actions = ActionSet{ActionClassify: true}

	status := pr.NewPRStatus()
	infos := make([]pr.PRInfo, prCount)
	indices := make([]int, prCount)
	for i := range prCount {
		infos[i] = pr.PRInfo{Owner: "org", Repo: "repo", Number: i + 1, Author: "renovate[bot]"}
		indices[i] = status.Add(infos[i])
	}

	var wg sync.WaitGroup
	for i := range prCount {
		wg.Go(func() { proc.ProcessPR(t.Context(), infos[i], status, indices[i]) })
	}
	wg.Wait()

	require.Equal(t, prCount, counted.count("pull"), "every PR is read on its own")

	require.Equal(t, 1, counted.count("required-status-checks"), "the base branch protection is read once for the repository")
	require.Equal(t, 1, counted.count("base-status"), "the base head statuses are read once for the base SHA")
	require.Equal(t, 1, counted.count("base-check-runs"), "the base head check runs are read once for the base SHA")

	for i := range prCount {
		require.Equal(t, pr.StatusFailed, status.StateAt(indices[i]), "PR %d", i+1)
	}
}

// ensureWriteAccess is the guard every write goes through, and it memoises
// its answer per repository. The PRs of one repository ask for it at the
// same time, so the first answer must serve all of them.
func TestEnsureWriteAccessIsReadOncePerRepository(t *testing.T) {
	const callers = 20
	counted, server := concurrentServer(t, 1)

	proc := NewProcessor(newTestClient(t, server), false, false, "marge")

	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() { errs[i] = proc.ensureWriteAccess(t.Context(), "org", "repo") })
	}
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
	}
	require.Equal(t, 1, counted.count("repository"))
}
