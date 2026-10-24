package cmd

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

type sentProgress struct {
	value   float64
	message string
}

type progressRecorder struct {
	mu   sync.Mutex
	sent []sentProgress
}

func (r *progressRecorder) send(value float64, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sentProgress{value: value, message: message})
}

func (r *progressRecorder) all() []sentProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentProgress(nil), r.sent...)
}

// Every report carries a higher value than the last and says where the run
// stands in PRs; nothing is reported once the call is over.
func TestRunProgress_reportsWhereTheRunStands(t *testing.T) {
	recorder := &progressRecorder{}
	progress := &runProgress{send: recorder.send}

	progress.heartbeat()
	progress.Planned(3)
	progress.Finished()
	progress.Finished()
	progress.Revisiting(0)
	progress.Revisiting(1)
	progress.Finished()
	progress.close()
	progress.heartbeat()

	require.Equal(t, []sentProgress{
		{1, "finding the PRs"},
		{2, "0 of 3 PRs done"},
		{3, "1 of 3 PRs done"},
		{4, "2 of 3 PRs done"},
		{5, "2 of 3 PRs done; waiting on 1 PRs a merge of this run made dirty"},
		{6, "3 of 3 PRs done; waiting on 1 PRs a merge of this run made dirty"},
	}, recorder.all())
}

func TestRunProgress_nilReportsNothing(t *testing.T) {
	var progress *runProgress
	progress.Planned(1)
	progress.Finished()
	progress.Revisiting(1)
	progress.close()
	require.Nil(t, progressFrom(t.Context()))
}

// A run with nothing new to say still reports every interval, and stops
// when the call returns.
func TestRunProgress_heartbeatsWhileTheRunIsQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder := &progressRecorder{}
		progress := &runProgress{send: recorder.send}

		stop := progress.start(progressInterval)
		require.Len(t, recorder.all(), 1, "the start is reported at once")

		time.Sleep(3*progressInterval + time.Second)
		synctest.Wait()
		require.Len(t, recorder.all(), 4)

		stop()
		time.Sleep(3 * progressInterval)
		synctest.Wait()
		require.Len(t, recorder.all(), 4, "nothing is reported after the call")
	})
}

// A caller that sends a progressToken over the streamable HTTP transport
// the chart runs receives the run's progress under that token before the
// result.
func TestReportProgress_reachesAStreamableHTTPCaller(t *testing.T) {
	tools, _ := teamsServer(t,
		map[string]string{"repositories/team-bumblebee.yaml": "- name: marge\n"},
		map[string][]*github.PullRequest{"giantswarm/marge": {botPull("marge", 1, "")}})
	handler, streamable := httpHandler(newMCPServer(tools.newClient))
	t.Cleanup(func() { streamable.CloseSessions(context.Background()) })
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	caller, err := client.NewStreamableHttpClient(srv.URL+mcpEndpoint,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer gho_test"}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = caller.Close() })

	var (
		mu       sync.Mutex
		received []mcp.JSONRPCNotification
	)
	caller.OnNotification(func(notification mcp.JSONRPCNotification) {
		if notification.Method != string(mcp.MethodNotificationProgress) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		received = append(received, notification)
	})
	require.NoError(t, caller.Start(t.Context()))
	initialize := mcp.InitializeRequest{}
	initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "0"}
	_, err = caller.Initialize(t.Context(), initialize)
	require.NoError(t, err)

	request := mcp.CallToolRequest{}
	request.Params.Name = "list"
	request.Params.Arguments = map[string]any{"team": "bumblebee", "refresh": true}
	request.Params.Meta = &mcp.Meta{ProgressToken: "page-1"}
	_, err = caller.CallTool(t.Context(), request)
	require.NoError(t, err)

	// The server queues a notification and writes the result itself, so
	// the last notification can reach the caller after the result.
	done := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, notification := range received {
			if notification.Params.AdditionalFields["message"] == "1 of 1 PRs done" {
				return true
			}
		}
		return false
	}
	require.Eventually(t, done, 5*time.Second, 10*time.Millisecond, "the caller hears that the PR is done")

	mu.Lock()
	defer mu.Unlock()
	last := 0.0
	for _, notification := range received {
		fields := notification.Params.AdditionalFields
		require.Equal(t, "page-1", fields["progressToken"])
		value, ok := fields["progress"].(float64)
		require.True(t, ok)
		require.Greater(t, value, last, "progress only increases")
		last = value
	}
}
