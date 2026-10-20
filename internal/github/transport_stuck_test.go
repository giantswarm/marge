package github

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stuckServer hears the first stuck requests out without answering them,
// then answers every later one.
func stuckServer(t *testing.T, stuck int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var arrived atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) <= stuck {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { close(release); server.Close() })
	return server, &arrived
}

func stuckTransport() retryStuck {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	return retryStuck{base: transport}
}

// A read that hears nothing is sent again, and the sweep reads on.
func TestRetryStuckSendsAStuckReadAgain(t *testing.T) {
	server, arrived := stuckServer(t, stuckRetries)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/repos/o/r/pulls/1", nil)
	require.NoError(t, err)

	resp, err := stuckTransport().RoundTrip(req)

	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, stuckRetries+1, arrived.Load())
}

// A read stuck past every retry reaches the caller as the timeout it is.
func TestRetryStuckGivesUpAfterItsRetries(t *testing.T) {
	server, arrived := stuckServer(t, stuckRetries+1)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/repos/o/r/pulls/1", nil)
	require.NoError(t, err)

	_, err = stuckTransport().RoundTrip(req)

	require.True(t, isTimeout(err), "a timeout, got %v", err)
	require.EqualValues(t, stuckRetries+1, arrived.Load())
}

// A write that timed out may have been performed, so it is never sent again.
func TestRetryStuckNeverSendsAWriteAgain(t *testing.T) {
	server, arrived := stuckServer(t, 1)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+"/repos/o/r/pulls/1/merge", strings.NewReader("{}"))
	require.NoError(t, err)

	_, err = stuckTransport().RoundTrip(req)

	require.True(t, isTimeout(err), "a timeout, got %v", err)
	require.EqualValues(t, 1, arrived.Load())
}
