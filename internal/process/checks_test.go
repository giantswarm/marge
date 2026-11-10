package process

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

var runStart = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// checkRunsServer serves one PR head (org/repo#1) with no commit statuses
// and the given check runs, the way GitHub lists them: every workflow run's
// check runs, in no order the reader may rely on.
func checkRunsServer(t *testing.T, runs []*github.CheckRun) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(github.CombinedStatus{SHA: new("abc123"), State: new("pending")})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(github.ListCheckRunsResults{Total: new(len(runs)), CheckRuns: runs})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// completedRun builds one finished check run: its own check suite (a new
// workflow run on the same head), started at the given time and completed
// a minute later.
func completedRun(id int64, name, conclusion string, started time.Time) *github.CheckRun {
	return &github.CheckRun{
		ID:          new(id),
		Name:        new(name),
		Status:      new(statusCompleted),
		Conclusion:  new(conclusion),
		StartedAt:   &github.Timestamp{Time: started},
		CompletedAt: &github.Timestamp{Time: started.Add(time.Minute)},
		CheckSuite:  &github.CheckSuite{ID: new(id)},
	}
}

func headOutcome(t *testing.T, runs []*github.CheckRun) checkOutcome {
	t.Helper()
	processor := &Processor{Client: newTestClient(t, checkRunsServer(t, runs))}
	outcome, err := processor.getCombinedCheckState(t.Context(), pr.PRInfo{Owner: "org", Repo: "repo", Number: 1})
	require.NoError(t, err)
	return outcome
}

// A workflow started twice on one head (Renovate's opened and edited events
// a second apart) lists its check once per run: the run the concurrency
// group cancelled and the runs that passed. The newest run is the verdict,
// so the cancelled one neither fails the PR nor leaves it without one.
func TestSupersededCancelledRunDoesNotCount(t *testing.T) {
	outcome := headOutcome(t, []*github.CheckRun{
		completedRun(1, "semantic-pull-request", conclusionCancelled, runStart),
		completedRun(2, "semantic-pull-request", stateSuccess, runStart.Add(time.Second)),
		completedRun(3, "semantic-pull-request", stateSuccess, runStart.Add(2*time.Second)),
	})

	require.Equal(t, stateSuccess, outcome.state)
	require.Empty(t, outcome.noVerdictChecks)
	require.Empty(t, outcome.failedChecks)
	require.Empty(t, outcome.pendingChecks)
	require.True(t, outcome.reported["semantic-pull-request"].green())
	require.Equal(t, runStart.Add(2*time.Second+time.Minute), outcome.settledAt)
}

// The newest run is the verdict in both directions: a check whose latest
// run was cancelled established nothing, however green an earlier run was.
func TestNewestCancelledRunIsNoVerdict(t *testing.T) {
	outcome := headOutcome(t, []*github.CheckRun{
		completedRun(1, "semantic-pull-request", stateSuccess, runStart),
		completedRun(2, "semantic-pull-request", conclusionCancelled, runStart.Add(time.Second)),
	})

	require.Equal(t, stateNoVerdict, outcome.state)
	require.Len(t, outcome.noVerdictChecks, 1)
	require.Equal(t, "semantic-pull-request", outcome.noVerdictChecks[0].Name)
	require.Equal(t, cancelledReason, outcome.noVerdictChecks[0].Reason)
	require.Empty(t, outcome.failedChecks)
	require.False(t, outcome.reported["semantic-pull-request"].green())
}

// A run that fails after a run of the same check passed is a failure of
// the code: the older success is no longer the verdict.
func TestNewestFailedRunIsAFailure(t *testing.T) {
	outcome := headOutcome(t, []*github.CheckRun{
		completedRun(1, "go-build", stateSuccess, runStart),
		completedRun(2, "go-build", stateFailure, runStart.Add(time.Second)),
	})

	require.Equal(t, stateFailure, outcome.state)
	require.Equal(t, []string{"go-build"}, outcome.failedChecks)
	require.True(t, outcome.reported["go-build"].failed)
	require.False(t, outcome.reported["go-build"].green())
}

// GitHub's listing order carries no meaning, and a run still in progress is
// the newest whatever it reports as its start: the check is pending even
// when an older run of it failed.
func TestNewestRunStillRunningIsPending(t *testing.T) {
	outcome := headOutcome(t, []*github.CheckRun{
		{
			ID:         new(int64(2)),
			Name:       new("go-build"),
			Status:     new("queued"),
			CheckSuite: &github.CheckSuite{ID: new(int64(2))},
		},
		completedRun(1, "go-build", stateFailure, runStart),
	})

	require.Equal(t, statePending, outcome.state)
	require.Empty(t, outcome.failedChecks)
	require.Len(t, outcome.pendingChecks, 1)
	require.Equal(t, "go-build", outcome.pendingChecks[0].Name)
}

// Two checks with one run each are judged as before, and only the one that
// was run twice is reduced to its newest run.
func TestNewestRunsKeepsOrderAndUnnamedRuns(t *testing.T) {
	runs := []*github.CheckRun{
		completedRun(1, "a", stateSuccess, runStart),
		completedRun(2, "b", stateSuccess, runStart),
		{ID: new(int64(3)), Status: new(statusCompleted), Conclusion: new(stateFailure)},
		completedRun(4, "a", stateSuccess, runStart.Add(time.Second)),
		{ID: new(int64(5)), Status: new(statusCompleted), Conclusion: new(stateFailure)},
	}

	got := newestRuns(runs)

	require.Len(t, got, 4)
	require.Equal(t, int64(4), got[0].GetID())
	require.Equal(t, "b", got[1].GetName())
	require.Equal(t, int64(3), got[2].GetID())
	require.Equal(t, int64(5), got[3].GetID())
}

func TestSupersedes(t *testing.T) {
	at := func(d time.Duration) *github.Timestamp { return &github.Timestamp{Time: runStart.Add(d)} }
	suite := func(id int64) *github.CheckSuite { return &github.CheckSuite{ID: new(id)} }
	tests := []struct {
		name  string
		newer *github.CheckRun
		older *github.CheckRun
	}{
		{"the run still in progress, whatever the times", &github.CheckRun{Status: new("queued")}, &github.CheckRun{Status: new(statusCompleted), StartedAt: at(time.Hour), CompletedAt: at(2 * time.Hour)}},
		{"the later start", &github.CheckRun{StartedAt: at(time.Second), CompletedAt: at(time.Minute)}, &github.CheckRun{StartedAt: at(0), CompletedAt: at(time.Hour)}},
		{"the later completion", &github.CheckRun{StartedAt: at(0), CompletedAt: at(2 * time.Second)}, &github.CheckRun{StartedAt: at(0), CompletedAt: at(time.Second)}},
		{"the newer check suite", &github.CheckRun{CheckSuite: suite(2)}, &github.CheckRun{CheckSuite: suite(1)}},
		{"the newer run", &github.CheckRun{ID: new(int64(2))}, &github.CheckRun{ID: new(int64(1))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.True(t, supersedes(tt.newer, tt.older))
			require.False(t, supersedes(tt.older, tt.newer))
		})
	}
}

// The base head is read the same way: a check whose first run on the base
// commit was cancelled and whose newer run passed is green there, so a PR
// failing that check behind the base is stale rather than failed.
func TestBaseContextStatesJudgeTheNewestRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/commits/bbb222/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(github.CombinedStatus{SHA: new("bbb222"), State: new("pending")})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/bbb222/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		runs := []*github.CheckRun{
			completedRun(1, "go-build", conclusionCancelled, runStart),
			completedRun(2, "go-build", stateSuccess, runStart.Add(time.Second)),
		}
		_ = json.NewEncoder(w).Encode(github.ListCheckRunsResults{Total: new(len(runs)), CheckRuns: runs})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	processor := &Processor{Client: newTestClient(t, server)}

	states, err := processor.readBaseContextStates(t.Context(), pr.PRInfo{Owner: "org", Repo: "repo"}, "bbb222")

	require.NoError(t, err)
	require.True(t, states["go-build"].green())
	require.Equal(t, runStart.Add(time.Second+time.Minute), states["go-build"].at)
}
