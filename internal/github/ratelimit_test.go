package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// secondaryRefusal is what GitHub answers a call it refused for rate: 403
// with the wait it wants. An access refusal carries neither header, which is
// how the two are told apart.
func secondaryRefusal(w http.ResponseWriter, seconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
}

// gated returns a transport whose waits are recorded rather than served, so
// a test asserts the wait without spending it.
func gated(t *testing.T, server *httptest.Server) (*rateLimited, *[]time.Duration) {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	clock := time.Now()

	transport := newRateLimited(http.DefaultTransport)
	transport.now = func() time.Time { return clock }
	transport.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		clock = clock.Add(d)
		mu.Unlock()
		return ctx.Err()
	}
	return transport, &waits
}

func request(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	return req
}

// awaitCount waits for counter to reach want. It fails the test rather than
// spinning for ever, so a transport that never closes its gate reports a
// failure and not a hung run.
func awaitCount(t *testing.T, counter *atomic.Int32, want int32, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for counter.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d of %d after 10s", what, counter.Load(), want)
		}
		runtime.Gosched()
	}
}

// A refusal for rate is the one answer worth sending again: GitHub refused
// the call rather than performing it, so the retry costs nothing that the
// first attempt already spent.
func TestRateLimitedRetriesAfterTheWaitGitHubNamed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			secondaryRefusal(w, 2)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	transport, waits := gated(t, server)
	resp, err := transport.RoundTrip(request(t, t.Context(), server.URL))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, []time.Duration{2 * time.Second}, *waits)
}

// An access refusal is not a rate refusal. A 403 that names no wait is the
// caller's answer and is passed through, not retried.
func TestRateLimitedPassesAnAccessRefusalThrough(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	t.Cleanup(server.Close)

	transport, waits := gated(t, server)
	resp, err := transport.RoundTrip(request(t, t.Context(), server.URL))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, int32(1), calls.Load())
	require.Empty(t, *waits)
}

// The gate is the point of putting this in the transport. A sweep puts a
// team's PRs on one token at once, so a refusal arrives while many other
// requests are in flight. Every one of them waits, not only the one that was
// refused: without that, the other callers keep sending while one waits,
// which is what earned the refusal.
func TestRateLimitedHoldsEveryRequestBehindTheGate(t *testing.T) {
	const others = 20
	var calls atomic.Int32
	var refusals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if refusals.Add(1) == 1 {
			secondaryRefusal(w, 60)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	var mu sync.Mutex
	clock := time.Now()
	var waiting atomic.Int32
	release := make(chan struct{})

	transport := newRateLimited(http.DefaultTransport)
	transport.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	transport.sleep = func(ctx context.Context, d time.Duration) error {
		waiting.Add(1)
		<-release
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
		return ctx.Err()
	}

	send := func() error {
		resp, err := transport.RoundTrip(request(t, t.Context(), server.URL))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, others+1)
	wg.Go(func() { errs[0] = send() })

	// The first request earns the refusal and shuts the gate for a minute.
	awaitCount(t, &waiting, 1, "the refused request never waited")
	sawWhenShut := calls.Load()

	for i := 1; i <= others; i++ {
		wg.Go(func() { errs[i] = send() })
	}
	awaitCount(t, &waiting, others+1, "requests held behind the gate")

	require.Equal(t, sawWhenShut, calls.Load(),
		"no request reached GitHub while the gate was shut")

	close(release)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(others+2), calls.Load(), "the refusal, its retry, and one call each")
}

// A sweep that is cancelled must not sit out a rate limit. The wait returns
// the caller's own error instead.
func TestRateLimitedStopsWaitingWhenTheCallerGivesUp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryRefusal(w, 30)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	transport := newRateLimited(http.DefaultTransport)
	transport.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	_, err := transport.RoundTrip(request(t, ctx, server.URL))
	require.ErrorIs(t, err, context.Canceled)
}

// A wait longer than the transport handles is the caller's to report. A
// sweep must not sleep through a primary limit that resets in an hour.
func TestRateLimitedDoesNotWaitOutALongLimit(t *testing.T) {
	var calls atomic.Int32
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	transport, waits := gated(t, server)
	resp, err := transport.RoundTrip(request(t, t.Context(), server.URL))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, int32(1), calls.Load())
	require.Empty(t, *waits)
}

// A refusal that outlives the retries reaches the caller. The sweep reports
// it rather than holding on.
func TestRateLimitedGivesUpAfterItsRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		secondaryRefusal(w, 1)
	}))
	t.Cleanup(server.Close)

	transport, _ := gated(t, server)
	resp, err := transport.RoundTrip(request(t, t.Context(), server.URL))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, int32(rateRetries+1), calls.Load())
}
