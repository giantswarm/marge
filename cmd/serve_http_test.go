package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHTTPHandler_probesAndInitialize drives the streamable HTTP transport
// the Helm chart runs: the probe paths answer 200, an MCP initialize on the
// endpoint answers with this server's name, and anything else is a 404.
func TestHTTPHandler_probesAndInitialize(t *testing.T) {
	handler, streamable := httpHandler(newMCPServer())
	t.Cleanup(func() { streamable.CloseSessions(context.Background()) })
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
			t.Fatalf("GET %s = %d %q, want 200 ok", path, resp.StatusCode, body)
		}
	}

	resp, err := http.Get(srv.URL + "/somewhere-else")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", resp.StatusCode)
	}

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+mcpEndpoint, bytes.NewBufferString(init))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST initialize: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d: %s", resp.StatusCode, body)
	}
	var reply struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("initialize reply is not JSON (%v): %s", err, body)
	}
	if reply.Result.ServerInfo.Name != "marge" {
		t.Fatalf("serverInfo.name = %q, want marge: %s", reply.Result.ServerInfo.Name, body)
	}
}

// TestServeHTTP_stopsOnContext starts the real listener on a free port and
// checks that cancelling the context shuts it down within the grace period.
func TestServeHTTP_stopsOnContext(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var log bytes.Buffer
	go func() { done <- serveHTTP(ctx, newMCPServer(), addr, &log) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveHTTP returned %v, want nil after a clean shutdown", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("serveHTTP did not return after the context was cancelled")
	}
	if !strings.Contains(log.String(), "serving MCP over streamable HTTP") {
		t.Fatalf("startup line missing from log: %q", log.String())
	}
}

// TestServeRejectsUnknownTransport keeps the flag validation honest.
func TestServeRejectsUnknownTransport(t *testing.T) {
	prev := serveOpts.transport
	t.Cleanup(func() { serveOpts.transport = prev })
	serveOpts.transport = "carrier-pigeon"
	err := serveCmd.RunE(serveCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Fatalf("RunE with an unknown transport = %v, want an unknown transport error", err)
	}
}
