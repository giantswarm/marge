package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/marge/internal/pr"
)

func watchFile(t *testing.T) (*binaryWatch, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "marge")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return &binaryWatch{path: path, start: info}, path
}

// replaceFile does what self-update does: renames a new file over the path.
func replaceFile(t *testing.T, path string) {
	t.Helper()
	next := path + ".new"
	if err := os.WriteFile(next, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryWatchReplaced(t *testing.T) {
	w, path := watchFile(t)
	if w.replaced() {
		t.Fatal("untouched binary reported as replaced")
	}
	replaceFile(t, path)
	if !w.replaced() {
		t.Fatal("renamed-over binary not reported as replaced")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !w.replaced() {
		t.Fatal("removed binary not reported as replaced")
	}
	if (*binaryWatch)(nil).replaced() {
		t.Fatal("a nil watch reported a replacement")
	}
}

func TestRestartGuard(t *testing.T) {
	staleExitGrace = time.Millisecond
	called := func(calls *atomic.Int32) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			return mcp.NewToolResultText("done"), nil
		}
	}

	t.Run("unchanged binary serves and keeps running", func(t *testing.T) {
		w, _ := watchFile(t)
		var calls, stops atomic.Int32
		handler := restartGuard(w, func() { stops.Add(1) }, &bytes.Buffer{})(called(&calls))
		answer, _ := handler(context.Background(), mcp.CallToolRequest{})
		time.Sleep(20 * time.Millisecond)
		if answer.IsError || calls.Load() != 1 || stops.Load() != 0 {
			t.Fatalf("answer error=%v calls=%d stops=%d", answer.IsError, calls.Load(), stops.Load())
		}
	})

	t.Run("replaced before the call refuses it and stops", func(t *testing.T) {
		w, path := watchFile(t)
		replaceFile(t, path)
		stopped := make(chan struct{})
		var calls atomic.Int32
		handler := restartGuard(w, func() { close(stopped) }, &bytes.Buffer{})(called(&calls))
		answer, _ := handler(context.Background(), mcp.CallToolRequest{})
		if !answer.IsError || calls.Load() != 0 {
			t.Fatalf("answer error=%v calls=%d", answer.IsError, calls.Load())
		}
		if text := answer.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "restart") {
			t.Fatalf("message does not name the restart: %q", text)
		}
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("server did not stop")
		}
	})

	t.Run("replaced during the call finishes it and stops", func(t *testing.T) {
		w, path := watchFile(t)
		stopped := make(chan struct{})
		handler := restartGuard(w, func() { close(stopped) }, &bytes.Buffer{})(
			func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				replaceFile(t, path)
				return mcp.NewToolResultText("done"), nil
			})
		answer, _ := handler(context.Background(), mcp.CallToolRequest{})
		if answer.IsError {
			t.Fatal("the call in flight was refused")
		}
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("server did not stop")
		}
	})
}

func TestRunningServers(t *testing.T) {
	root := t.TempDir()
	prev := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = prev })

	proc := func(pid, exe, cmdline string) {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proc("11", "/bin/marge (deleted)", "marge\x00serve\x00")
	proc("12", "/bin/marge (deleted)", "marge\x00sweep\x00")
	proc("13", "/bin/marge", "marge\x00serve\x00")
	proc("14", "/bin/other (deleted)", "other\x00serve\x00")

	pids := runningServers("/bin/marge")
	if len(pids) != 1 || pids[0] != 11 {
		t.Fatalf("runningServers = %v, want [11]", pids)
	}
}

func TestSweepResultNamesTheVersion(t *testing.T) {
	prev := version
	version = "9.9.9"
	t.Cleanup(func() { version = prev })

	body, err := json.Marshal(buildSweepResult(pr.NewPRStatus(), nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"marge_version":"9.9.9"`) {
		t.Fatalf("sweep JSON lacks marge_version: %s", body)
	}
}
