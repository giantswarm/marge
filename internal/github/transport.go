package github

import (
	"net/http"
	"sync"
)

// idleConnsPerHost is how many idle connections to api.github.com the pool
// keeps. Go's default is 2, so a discovery that runs more requests than that
// at once re-dials and re-handshakes all but two of them. Every GitHub call
// marge makes goes to one host, so the pool is sized for the widest fan-out
// a sweep runs, not for a host count.
const idleConnsPerHost = 64

var (
	apiTransportOnce sync.Once
	apiTransport     *http.Transport
)

// APITransport returns the connection pool every GitHub client shares. One
// pool across the process is what lets a second tool call reuse the
// connections the first one opened.
func APITransport() *http.Transport {
	apiTransportOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 4 * idleConnsPerHost
		transport.MaxIdleConnsPerHost = idleConnsPerHost
		apiTransport = transport
	})
	return apiTransport
}
