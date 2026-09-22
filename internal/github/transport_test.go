package github

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAPITransport keeps the pool wide enough for the fan-out and shared
// across clients: a per-client pool would re-dial on every tool call.
func TestAPITransport(t *testing.T) {
	transport := APITransport()

	require.Equal(t, idleConnsPerHost, transport.MaxIdleConnsPerHost)
	require.GreaterOrEqual(t, transport.MaxIdleConns, idleConnsPerHost)
	require.Same(t, transport, APITransport())
	require.NotSame(t, http.DefaultTransport, transport)
}

// TestNewAppClientUsesTheSharedTransport: the scheduled sweep authenticates
// as the App, and its requests go through the App transport, so the pool and
// the rate gate have to sit under that one too. An App path that skipped the
// gate would keep sending while every other caller waits.
func TestNewAppClientUsesTheSharedTransport(t *testing.T) {
	client, err := NewAppClient(&App{}, defaultBaseURL, nil)
	require.NoError(t, err)

	transport, ok := client.Client().Transport.(*appTransport)
	require.True(t, ok)
	require.Same(t, SharedTransport(), transport.base)
}

// TestSharedTransportWrapsThePool keeps both properties in one place: every
// client shares one gate, and the pool is still underneath it.
func TestSharedTransportWrapsThePool(t *testing.T) {
	shared := SharedTransport()
	require.Same(t, shared, SharedTransport())

	gated, ok := shared.(*rateLimited)
	require.True(t, ok)
	require.Same(t, APITransport(), gated.base)
}
