package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// heldServer holds every request until release is closed and counts the
// writes in flight, so a test sees how many the transport let through.
type heldServer struct {
	release     chan struct{}
	arrived     atomic.Int32
	inflight    atomic.Int32
	maxInflight atomic.Int32
}

func (s *heldServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := s.inflight.Add(1)
		defer s.inflight.Add(-1)
		for seen := s.maxInflight.Load(); now > seen && !s.maxInflight.CompareAndSwap(seen, now); seen = s.maxInflight.Load() {
		}
		s.arrived.Add(1)
		<-s.release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server
}

func send(t *testing.T, ctx context.Context, method, url string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	require.NoError(t, err)
	resp, err := newRateLimited(http.DefaultTransport).RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// A run that reads wide holds its writes to the bound it was given, while
// its reads go on beside them.
func TestWriteBoundHoldsWritesAndLetsReadsThrough(t *testing.T) {
	const writes, reads, bound = 6, 4, 2
	server := &heldServer{release: make(chan struct{})}
	url := server.start(t).URL
	ctx := WithWriteBound(t.Context(), bound)

	var wg sync.WaitGroup
	for range writes {
		wg.Go(func() { require.NoError(t, send(t, ctx, http.MethodPost, url+"/repos/o/r/issues/1/labels")) })
	}
	for range reads {
		wg.Go(func() { require.NoError(t, send(t, ctx, http.MethodGet, url+"/repos/o/r/pulls/1")) })
	}
	wg.Go(func() { require.NoError(t, send(t, ctx, http.MethodPost, url+"/graphql")) })

	awaitCount(t, &server.arrived, bound+reads+1, "the bounded writes, every read and the GraphQL query")
	// Long enough for a write past the bound to arrive, were it let through.
	time.Sleep(50 * time.Millisecond)
	close(server.release)
	wg.Wait()

	require.EqualValues(t, bound+reads+1, server.maxInflight.Load(), "a write past the bound waits for a slot")
	require.EqualValues(t, writes+reads+1, server.arrived.Load(), "every request is sent")
}

// A run without a bound, a served read for one, sends its writes as they
// come.
func TestNoWriteBoundHoldsNothing(t *testing.T) {
	const writes = 4
	server := &heldServer{release: make(chan struct{})}
	url := server.start(t).URL

	var wg sync.WaitGroup
	for range writes {
		wg.Go(func() { require.NoError(t, send(t, t.Context(), http.MethodPut, url+"/repos/o/r/pulls/1/merge")) })
	}
	awaitCount(t, &server.arrived, writes, "every write")
	close(server.release)
	wg.Wait()
}

// A write cancelled while it waits for a slot is never sent.
func TestWriteBoundGivesUpWhenCancelled(t *testing.T) {
	server := &heldServer{release: make(chan struct{})}
	url := server.start(t).URL
	ctx := WithWriteBound(t.Context(), 1)

	var wg sync.WaitGroup
	wg.Go(func() { require.NoError(t, send(t, ctx, http.MethodPost, url+"/repos/o/r/issues/1/comments")) })
	awaitCount(t, &server.arrived, 1, "the write that holds the slot")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, send(t, cancelled, http.MethodPost, url+"/repos/o/r/issues/2/comments"), context.Canceled)

	close(server.release)
	wg.Wait()
	require.EqualValues(t, 1, server.arrived.Load())
}
