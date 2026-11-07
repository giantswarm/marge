package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// gatewayServer serves POST /notices and records what it received, so the
// client is tested against real protocol traffic.
type gatewayServer struct {
	*httptest.Server
	auth    string
	request notice
}

func newGatewayServer(t *testing.T, status int, body string) *gatewayServer {
	t.Helper()
	server := &gatewayServer{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /notices", func(w http.ResponseWriter, r *http.Request) {
		server.auth = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&server.request))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})

	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// tokenFile writes a projected token the way the kubelet does.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(token+"\n"), 0o600))
	return path
}

func client(t *testing.T, server *gatewayServer) *Client {
	t.Helper()
	return &Client{BaseURL: server.URL, TokenFile: tokenFile(t, "sa-token"), HTTP: server.Client()}
}

// TestPost sends the summary as the pod's own ServiceAccount, to the channel
// the policy names.
func TestPost(t *testing.T) {
	server := newGatewayServer(t, http.StatusCreated, `{"channel":"C0ALXPMB1PW","ts":"1.0"}`)

	require.NoError(t, client(t, server).Post(t.Context(), "bumblebee", "C0ALXPMB1PW", "2 merged"))

	require.Equal(t, "Bearer sa-token", server.auth)
	require.Equal(t, "bumblebee", server.request.Team)
	require.Equal(t, "C0ALXPMB1PW", server.request.Channel)
	require.Equal(t, "2 merged", server.request.Text)
}

// TestPost_gatewayRefusalIsAnError carries the gateway's own reason, so a run
// that could not post says why.
func TestPost_gatewayRefusalIsAnError(t *testing.T) {
	server := newGatewayServer(t, http.StatusForbidden, "this ServiceAccount may not post team reviews")

	err := client(t, server).Post(t.Context(), "bumblebee", "C0ALXPMB1PW", "2 merged")
	require.ErrorContains(t, err, "403")
	require.ErrorContains(t, err, "may not post")
}

// TestPost_refusesWhatTheGatewayWouldRefuse keeps the channel worth reading
// and fails a name before it reaches the gateway.
func TestPost_refusesWhatTheGatewayWouldRefuse(t *testing.T) {
	server := newGatewayServer(t, http.StatusCreated, "")
	posting := client(t, server)

	require.ErrorContains(t, posting.Post(t.Context(), "bumblebee", "C0ALXPMB1PW", "  "), "empty summary")
	require.ErrorContains(t, posting.Post(t.Context(), "bumblebee", "standup-bumblebee", "2 merged"), "not a channel ID")
	require.ErrorContains(t, posting.Post(t.Context(), "bumblebee", "", "2 merged"), "not a channel ID")
	require.Empty(t, server.request.Channel)
}

// TestPost_missingTokenNeverReachesTheGateway holds that the token is the
// credential: without it there is nothing to post with.
func TestPost_missingTokenNeverReachesTheGateway(t *testing.T) {
	server := newGatewayServer(t, http.StatusCreated, "")
	posting := &Client{BaseURL: server.URL, HTTP: server.Client()}

	require.ErrorContains(t, posting.Post(t.Context(), "bumblebee", "C0ALXPMB1PW", "2 merged"), TokenFileEnv)
	require.Empty(t, server.request.Channel)
}

// TestPost_trimsASummaryTheGatewayWouldRefuse holds that a long run still
// posts, on whole lines, and says how many lines it dropped.
func TestPost_trimsASummaryTheGatewayWouldRefuse(t *testing.T) {
	server := newGatewayServer(t, http.StatusCreated, "")
	long := strings.Repeat("• giantswarm/marge#1234 bumped a dependency of the sweep\n", 100)

	require.NoError(t, client(t, server).Post(t.Context(), "bumblebee", "C0ALXPMB1PW", long))

	require.LessOrEqual(t, len(server.request.Text), TextMax)
	require.Contains(t, server.request.Text, "more lines did not fit")
	require.True(t, strings.HasPrefix(server.request.Text, "• giantswarm/marge#1234"))
}

// TestLoadClient returns nothing when no gateway is named, so a sweep run by
// hand posts nothing.
func TestLoadClient(t *testing.T) {
	t.Setenv(URLEnv, "")
	require.Nil(t, LoadClient())

	t.Setenv(URLEnv, "http://klaus-gateway.agent-platform.svc:8080/")
	t.Setenv(TokenFileEnv, "/var/run/secrets/klaus-gateway/token")
	loaded := LoadClient()
	require.NotNil(t, loaded)
	require.Equal(t, "http://klaus-gateway.agent-platform.svc:8080", loaded.BaseURL)
	require.Equal(t, "/var/run/secrets/klaus-gateway/token", loaded.TokenFile)
}

// scriptedGateway answers each POST /notices with the next status of its
// script, and the last one for every post after it, and counts the posts.
type scriptedGateway struct {
	*httptest.Server
	posts atomic.Int32
}

func newScriptedGateway(t *testing.T, statuses ...int) *scriptedGateway {
	t.Helper()
	gateway := &scriptedGateway{}
	gateway.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(gateway.posts.Add(1))
		status := statuses[min(n, len(statuses))-1]
		w.WriteHeader(status)
		if status != http.StatusCreated {
			_, _ = w.Write([]byte("posting the notice failed"))
		}
	}))
	t.Cleanup(gateway.Close)
	return gateway
}

func scriptedClient(t *testing.T, gateway *scriptedGateway) *Client {
	t.Helper()
	return &Client{BaseURL: gateway.URL, TokenFile: tokenFile(t, "sa-token"), HTTP: gateway.Client(), Backoff: time.Millisecond}
}

// TestPost_retriesAGatewayThatIsDownForAMoment carries a notice over one
// 502, which is what a gateway or Slack hiccup answers.
func TestPost_retriesAGatewayThatIsDownForAMoment(t *testing.T) {
	gateway := newScriptedGateway(t, http.StatusBadGateway, http.StatusCreated)

	require.NoError(t, scriptedClient(t, gateway).Post(t.Context(), "honeybadger", "C0ALXPMB1PW", "2 merged"))
	require.EqualValues(t, 2, gateway.posts.Load())
}

// TestPost_givesUpOnAGatewayThatStaysDown bounds the retries and says the
// gateway was unavailable, so the caller can tell it from a refusal.
func TestPost_givesUpOnAGatewayThatStaysDown(t *testing.T) {
	gateway := newScriptedGateway(t, http.StatusServiceUnavailable)

	err := scriptedClient(t, gateway).Post(t.Context(), "honeybadger", "C0ALXPMB1PW", "2 merged")
	require.ErrorContains(t, err, "503")
	require.ErrorContains(t, err, "posting the notice failed")
	require.ErrorContains(t, err, "gave up after 4 attempts")
	require.True(t, Unavailable(err))
	require.EqualValues(t, attempts, gateway.posts.Load())
}

// TestPost_doesNotRetryARefusal: a 4xx is the token, the channel or the
// request, and the same post answers the same on every try.
func TestPost_doesNotRetryARefusal(t *testing.T) {
	gateway := newScriptedGateway(t, http.StatusForbidden, http.StatusCreated)

	err := scriptedClient(t, gateway).Post(t.Context(), "honeybadger", "C0ALXPMB1PW", "2 merged")
	require.ErrorContains(t, err, "403")
	require.False(t, Unavailable(err))
	require.EqualValues(t, 1, gateway.posts.Load())
}

// TestPost_retriesATimeout treats a gateway that does not answer in time
// like one that answered 5xx.
func TestPost_retriesATimeout(t *testing.T) {
	var posts atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	httpClient := server.Client()
	httpClient.Timeout = 50 * time.Millisecond
	posting := &Client{BaseURL: server.URL, TokenFile: tokenFile(t, "sa-token"), HTTP: httpClient, Backoff: time.Millisecond}

	require.NoError(t, posting.Post(t.Context(), "honeybadger", "C0ALXPMB1PW", "2 merged"))
	require.EqualValues(t, 2, posts.Load())
}

// TestPost_stopsRetryingWhenTheContextEnds keeps the stop notice of a
// terminating pod inside its grace period.
func TestPost_stopsRetryingWhenTheContextEnds(t *testing.T) {
	gateway := newScriptedGateway(t, http.StatusBadGateway)
	posting := scriptedClient(t, gateway)
	posting.Backoff = time.Hour

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := posting.Post(ctx, "honeybadger", "C0ALXPMB1PW", "2 merged")
	require.ErrorContains(t, err, "502")
	require.True(t, Unavailable(err))
	require.EqualValues(t, 1, gateway.posts.Load())
}
