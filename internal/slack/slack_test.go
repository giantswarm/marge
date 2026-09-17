package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// slackServer serves chat.postMessage and records what it received, so the
// client is tested against real protocol traffic.
type slackServer struct {
	*httptest.Server
	auth    string
	request postRequest
}

func newSlackServer(t *testing.T, answer postResponse) *slackServer {
	t.Helper()
	server := &slackServer{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		server.auth = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&server.request))
		_ = json.NewEncoder(w).Encode(answer)
	})

	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func client(server *slackServer) *Client {
	return &Client{Token: "test-bot-token", APIURL: server.URL + "/", HTTP: server.Client()}
}

// TestPost sends the summary with the bot token and the channel the policy
// names.
func TestPost(t *testing.T) {
	server := newSlackServer(t, postResponse{OK: true})

	require.NoError(t, client(server).Post(t.Context(), "#team-bumblebee", "2 merged"))

	require.Equal(t, "Bearer test-bot-token", server.auth)
	require.Equal(t, "#team-bumblebee", server.request.Channel)
	require.Equal(t, "2 merged", server.request.Text)
}

// TestPost_slackErrorIsAnError holds that Slack reports a failure with HTTP
// 200 and ok false, so the status alone never decides.
func TestPost_slackErrorIsAnError(t *testing.T) {
	server := newSlackServer(t, postResponse{OK: false, Error: "channel_not_found"})

	err := client(server).Post(t.Context(), "#gone", "2 merged")
	require.ErrorContains(t, err, "channel_not_found")
}

// TestPost_refusesAnEmptySummary keeps the channel worth reading: silence is
// the contract when a run changed nothing.
func TestPost_refusesAnEmptySummary(t *testing.T) {
	server := newSlackServer(t, postResponse{OK: true})

	require.ErrorContains(t, client(server).Post(t.Context(), "#team", "  "), "empty summary")
	require.ErrorContains(t, client(server).Post(t.Context(), "", "2 merged"), "no Slack channel")
	require.Empty(t, server.request.Channel)
}

// TestLoadClient returns nothing when no token is set, so a sweep run by
// hand posts nothing.
func TestLoadClient(t *testing.T) {
	t.Setenv(TokenEnv, "")
	require.Nil(t, LoadClient())

	t.Setenv(TokenEnv, "test-bot-token")
	loaded := LoadClient()
	require.NotNil(t, loaded)
	require.Equal(t, "test-bot-token", loaded.Token)
}
