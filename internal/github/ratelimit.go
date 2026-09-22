package github

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// rateRetries is how many times one request is sent again after GitHub
	// refused it for rate. A refusal that outlives them reaches the caller,
	// which reports it rather than holding the sweep.
	rateRetries = 3
	// maxRateWait bounds one wait. A primary limit can reset an hour away,
	// and a sweep must not sleep through it: a wait longer than this is
	// returned to the caller as the refusal it is.
	maxRateWait = time.Minute
)

// headerRetryAfter and the rate headers are what GitHub answers a refusal
// with. A 403 that carries none of them is an access refusal, not a rate
// one, and is passed through untouched.
const (
	headerRetryAfter    = "Retry-After"
	headerRateRemaining = "X-RateLimit-Remaining"
	headerRateReset     = "X-RateLimit-Reset"
)

// rateLimited retries a request GitHub refused for rate, after the time
// GitHub named.
//
// The gate is why it is one transport and not a per-request retry. A sweep
// puts a whole team's PRs on one token at once, so a refusal arrives at
// many requests together. Without the gate the others keep sending while
// one waits, which is what earned the refusal. While the gate is closed
// every request of the process waits for it.
type rateLimited struct {
	base http.RoundTripper

	mu sync.Mutex
	// openAt is when the gate opens. A zero time is an open gate.
	openAt time.Time
	// now is the clock, so a test does not wait in real time.
	now func() time.Time
	// sleep waits for d, or returns ctx's error when the caller gives up
	// first. A test replaces it so it does not wait in real time.
	sleep func(ctx context.Context, d time.Duration) error
}

func newRateLimited(base http.RoundTripper) *rateLimited {
	return &rateLimited{base: base, now: time.Now, sleep: sleepUntil}
}

// sleepUntil waits for d, or returns ctx's error when the caller gives up
// first. A sweep that is cancelled must not sit out a rate limit.
func sleepUntil(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *rateLimited) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := r.waitForGate(req); err != nil {
			return nil, err
		}

		resp, err := r.base.RoundTrip(req)
		if err != nil || attempt == rateRetries {
			return resp, err
		}

		wait, refused := r.rateRefusal(resp)
		if !refused || !rewindable(req) {
			return resp, nil
		}

		// The request was refused, not performed, so sending it again is
		// safe whatever its method.
		drain(resp)
		r.closeGate(wait)
	}
}

// rateRefusal reports how long GitHub asked us to wait, and whether the
// answer was a refusal for rate at all. A wait longer than maxRateWait is
// not a refusal this transport handles.
func (r *rateLimited) rateRefusal(resp *http.Response) (time.Duration, bool) {
	if resp == nil || (resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests) {
		return 0, false
	}

	wait, named := retryAfter(resp.Header, r.now())
	if !named || wait > maxRateWait {
		return 0, false
	}
	return wait, true
}

// retryAfter reads the wait GitHub named, from Retry-After for a secondary
// limit and from the reset time for an exhausted primary one.
func retryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	if raw := header.Get(headerRetryAfter); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second, true
		}
	}
	if header.Get(headerRateRemaining) != "0" {
		return 0, false
	}
	raw := header.Get(headerRateReset)
	if raw == "" {
		return 0, false
	}
	reset, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return max(time.Unix(reset, 0).Sub(now), 0), true
}

// closeGate holds every request of the process until the wait has passed. A
// gate already closed for longer is left alone.
func (r *rateLimited) closeGate(wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if until := r.now().Add(wait); until.After(r.openAt) {
		r.openAt = until
	}
}

// waitForGate blocks until the gate is open, or the request is cancelled.
func (r *rateLimited) waitForGate(req *http.Request) error {
	for {
		r.mu.Lock()
		wait := r.openAt.Sub(r.now())
		r.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		if err := r.sleep(req.Context(), wait); err != nil {
			return err
		}
	}
}

// rewindable reports whether the request can be sent again. A body the
// caller cannot replay is not retried, because half of it is already gone.
func rewindable(req *http.Request) bool {
	if req.Body == nil || req.GetBody == nil {
		return req.Body == nil
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	req.Body = body
	return true
}

// drain reads and closes a response this transport discards, so its
// connection goes back to the pool.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}
