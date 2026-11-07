package github

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// idleConnsPerHost is how many idle connections to api.github.com the pool
// keeps. Go's default is 2, so a discovery that runs more requests than that
// at once re-dials and re-handshakes all but two of them. Every GitHub call
// marge makes goes to one host, so the pool is sized for the widest fan-out
// a sweep runs, not for a host count.
const idleConnsPerHost = 64

// responseHeaderTimeout bounds how long one GitHub request waits for its
// answer to begin. GitHub answers in a few seconds; a request that hears
// nothing for this long is stuck, and without a bound it holds its PR, and
// the repository's slot, for as long as the connection stays open.
const responseHeaderTimeout = 30 * time.Second

// stuckRetries is how many times a stuck read is sent again.
const stuckRetries = 2

var (
	apiTransportOnce sync.Once
	apiTransport     *http.Transport

	sharedTransportOnce sync.Once
	sharedTransport     http.RoundTripper
)

// APITransport returns the connection pool every GitHub client shares. One
// pool across the process is what lets a second tool call reuse the
// connections the first one opened.
func APITransport() *http.Transport {
	apiTransportOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 4 * idleConnsPerHost
		transport.MaxIdleConnsPerHost = idleConnsPerHost
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		apiTransport = transport
	})
	return apiTransport
}

// SharedTransport returns the round tripper every GitHub client in the
// process uses: the pool, behind the rate gate. It is one gate because a
// refusal for rate is about the token, not about the request, so a sweep
// that earned one must hold every call it has in flight and not only the
// one that was refused.
func SharedTransport() http.RoundTripper {
	sharedTransportOnce.Do(func() {
		sharedTransport = newRateLimited(retryStuck{base: APITransport()})
	})
	return sharedTransport
}

// retryStuck sends a read again when it timed out waiting for its answer. A
// read changes nothing, so a second one is safe; a write is never sent
// again, because a write that timed out may have been performed, and its
// error reaches the caller.
type retryStuck struct {
	base http.RoundTripper
}

func (r retryStuck) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := r.base.RoundTrip(req)
		if err == nil || attempt == stuckRetries || !isTimeout(err) || req.Context().Err() != nil ||
			isWrite(req) || !rewindable(req) {
			return resp, err
		}
	}
}

// isTimeout reports whether err is a request that ran out of time.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
