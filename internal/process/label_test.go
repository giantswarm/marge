package process

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// labelServer accepts label writes on any PR of org/repo and records how
// many were in flight at once.
type labelServer struct {
	inflight    atomic.Int32
	maxInflight atomic.Int32
	writes      atomic.Int32
}

func (s *labelServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/org/repo/issues/{number}/labels", func(w http.ResponseWriter, _ *http.Request) {
		now := s.inflight.Add(1)
		defer s.inflight.Add(-1)
		for {
			seen := s.maxInflight.Load()
			if now <= seen || s.maxInflight.CompareAndSwap(seen, now) {
				break
			}
		}
		// Long enough that unbounded writers overlap.
		time.Sleep(10 * time.Millisecond)
		s.writes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*github.Label{{Name: pr.LabelPrefix + "failed"}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func labelRun(status *pr.PRStatus, number int) *prRun {
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: number}
	return &prRun{info: info, pull: &github.PullRequest{}, status: status, idx: status.Add(info)}
}

// A sweep that reads wider than its policy lets it write still holds its
// label writes to the bound it was given.
func TestSetLabelHoldsLabelWritesToTheirBound(t *testing.T) {
	const writers, bound = 12, 2
	server := &labelServer{}
	proc := NewProcessor(newTestClient(t, server.start(t)), false, false, "marge")
	proc.LabelWrites = NewSlots(bound)

	status := pr.NewPRStatus()
	runs := make([]*prRun, writers)
	for i := range writers {
		runs[i] = labelRun(status, i+1)
	}

	var wg sync.WaitGroup
	for _, run := range runs {
		wg.Go(func() { proc.setLabel(t.Context(), run, "failed") })
	}
	wg.Wait()

	require.EqualValues(t, writers, server.writes.Load(), "every PR is labelled")
	require.LessOrEqual(t, server.maxInflight.Load(), int32(bound))
	for _, run := range runs {
		require.Empty(t, run.notes)
	}
}

// A sweep cancelled while its label write waits for a slot writes nothing
// and says why.
func TestSetLabelGivesUpWhenCancelledWaitingForASlot(t *testing.T) {
	server := &labelServer{}
	proc := NewProcessor(newTestClient(t, server.start(t)), false, false, "marge")
	proc.LabelWrites = NewSlots(1)
	proc.LabelWrites <- struct{}{}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	run := labelRun(pr.NewPRStatus(), 1)
	proc.setLabel(ctx, run, "failed")

	require.Zero(t, server.writes.Load())
	require.Equal(t, []string{"label not set: context canceled"}, run.notes)
}
