package github

import (
	"context"
	"net/http"
	"strings"
)

type writeBoundKey struct{}

// WithWriteBound returns a context under which at most n GitHub writes are
// in flight at once, however many reads run beside them. GitHub answers a
// burst of writes with a secondary rate limit, so a sweep reads as wide as
// GitHub answers and holds its writes to the policy's ceiling. The bound is
// the run's, not the process's: two sweeps under one served process each
// keep their own.
func WithWriteBound(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, writeBoundKey{}, make(chan struct{}, max(n, 1)))
}

// acquireWrite takes a write slot for req when its context carries a bound
// and req is a write, or returns the context's error when the caller gives
// up first. release gives the slot back.
func acquireWrite(req *http.Request) (release func(), err error) {
	slots, bounded := req.Context().Value(writeBoundKey{}).(chan struct{})
	if !bounded || !isWrite(req) {
		return func() {}, nil
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

// isWrite reports whether req changes something on GitHub. A GraphQL query is
// a POST that reads, and minting an installation token is a POST that writes
// nothing a person sees; neither spends the write bound.
func isWrite(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	path := req.URL.Path
	return !strings.HasSuffix(path, "/graphql") && !strings.HasSuffix(path, "/access_tokens")
}
