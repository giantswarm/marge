package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// requestsAtOnce bounds the requests one bucket has in flight. GitHub
	// refuses a token more than 100 concurrent requests as a secondary
	// limit, and a sweep of several teams at once fans out over every
	// team's PRs together.
	requestsAtOnce = 64
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
// one waits, which is what earned the refusal. While a bucket's gate is
// closed every request drawing on that bucket waits for it; a request on
// another bucket, which is another person's token in a served process, does
// not.
type rateLimited struct {
	base http.RoundTripper

	mu sync.Mutex
	// gates holds when each bucket's gate opens. A bucket with no entry, or
	// an entry in the past, has an open gate.
	gates map[string]time.Time
	// inFlight holds a slot per request each bucket has in flight, at
	// most requestsAtOnce.
	inFlight map[string]chan struct{}
	// now is the clock, so a test does not wait in real time.
	now func() time.Time
	// sleep waits for d, or returns ctx's error when the caller gives up
	// first. A test replaces it so it does not wait in real time.
	sleep func(ctx context.Context, d time.Duration) error
}

func newRateLimited(base http.RoundTripper) *rateLimited {
	return &rateLimited{
		base:     base,
		gates:    make(map[string]time.Time),
		inFlight: make(map[string]chan struct{}),
		now:      time.Now,
		sleep:    sleepUntil,
	}
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
	release, err := acquireWrite(req)
	if err != nil {
		return nil, err
	}
	defer release()

	bucket := rateBucketOf(req)
	done, err := r.acquire(req, bucket)
	if err != nil {
		return nil, err
	}
	defer done()

	for attempt := 0; ; attempt++ {
		if err := r.waitForGate(req, bucket); err != nil {
			return nil, err
		}

		resp, err := r.base.RoundTrip(req)
		if err != nil {
			return resp, err
		}

		wait, refused := rateRefusal(resp, r.now())
		if !refused {
			return resp, nil
		}
		if wait > maxRateWait {
			reportExhausted(req.Context(), r.now().Add(wait))
			return resp, nil
		}
		if attempt == rateRetries || !rewindable(req) {
			return resp, nil
		}

		// The request was refused, not performed, so sending it again is
		// safe whatever its method.
		drain(resp)
		r.closeGate(bucket, wait)
	}
}

// acquire takes one of the bucket's slots for req, or returns the request's
// error when the caller gives up first. done gives the slot back.
func (r *rateLimited) acquire(req *http.Request, bucket string) (done func(), err error) {
	r.mu.Lock()
	slots, ok := r.inFlight[bucket]
	if !ok {
		slots = make(chan struct{}, requestsAtOnce)
		r.inFlight[bucket] = slots
	}
	r.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

// rateRefusal reports whether the answer was a refusal for rate, and how
// long GitHub asked us to wait.
func rateRefusal(resp *http.Response, now time.Time) (time.Duration, bool) {
	if resp == nil || (resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests) {
		return 0, false
	}
	return retryAfter(resp.Header, now)
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

// closeGate holds every request of the bucket until the wait has passed. A
// gate already closed for longer is left alone.
func (r *rateLimited) closeGate(bucket string, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if until := r.now().Add(wait); until.After(r.gates[bucket]) {
		r.gates[bucket] = until
	}
}

// waitForGate blocks until the bucket's gate is open, or the request is
// cancelled. An open gate's entry is dropped, so the map holds only the
// buckets that are waiting.
func (r *rateLimited) waitForGate(req *http.Request, bucket string) error {
	for {
		r.mu.Lock()
		wait := r.gates[bucket].Sub(r.now())
		if wait <= 0 {
			delete(r.gates, bucket)
		}
		r.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		if err := r.sleep(req.Context(), wait); err != nil {
			return err
		}
	}
}

type rateBucketKey struct{}

// withRateBucket names the rate bucket a request draws on, for a credential
// that does not identify its bucket by itself: the App mints a token per
// repository scope, and every one of them spends the installation's limit.
func withRateBucket(req *http.Request, bucket string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), rateBucketKey{}, bucket))
}

// rateBucketOf is the bucket the request spends: the one it was given, or
// else its credential's. The credential is kept as a digest, so the gate
// holds no token.
func rateBucketOf(req *http.Request) string {
	if bucket, ok := req.Context().Value(rateBucketKey{}).(string); ok {
		return bucket
	}
	sum := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:])
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
