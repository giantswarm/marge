package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// staleExitGrace is how long a server that found its binary replaced waits
// before it stops, so the response of the call it is finishing reaches the
// host first.
var staleExitGrace = time.Second

// binaryWatch remembers the executable a server started from. marge
// self-update renames the new binary over that path, so the path then names
// another file than the one this process runs.
type binaryWatch struct {
	path  string
	start os.FileInfo
}

// watchBinary records the running executable. It returns nil when the
// executable cannot be identified: such a server serves as it always did.
func watchBinary() *binaryWatch {
	path, err := os.Executable()
	if err != nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &binaryWatch{path: path, start: info}
}

// replaced reports whether the file at the executable's path is no longer
// the one this process started from, or is gone.
func (w *binaryWatch) replaced() bool {
	if w == nil {
		return false
	}
	current, err := os.Stat(w.path)
	return err != nil || !os.SameFile(w.start, current)
}

// restartGuard makes a server stop serving code that is no longer on disk.
// A call that finds the binary already replaced is refused with the restart
// named; a call during which it was replaced is finished and answered. Either
// way the server stops after staleExitGrace, so the host that started it
// starts the new binary.
func restartGuard(w *binaryWatch, stop func(), log io.Writer) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if w.replaced() {
				scheduleStop(stop, log)
				return mcp.NewToolResultError(fmt.Sprintf(
					"marge %s was replaced by self-update and is stopping: call again once the host has restarted marge serve on the new binary", version)), nil
			}
			answer, err := next(ctx, request)
			if w.replaced() {
				scheduleStop(stop, log)
			}
			return answer, err
		}
	}
}

func scheduleStop(stop func(), log io.Writer) {
	_, _ = fmt.Fprintln(log, "the marge binary was replaced; stopping so the host restarts it")
	time.AfterFunc(staleExitGrace, stop)
}

// procRoot is where running processes are read from; a test points it at a
// fixture.
var procRoot = "/proc"

// runningServers returns the pids of `marge serve` processes whose
// executable was at path before an update replaced it. Without /proc the
// answer is empty.
func runningServers(path string) []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, entry.Name())
		target, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil || target != path+" (deleted)" {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		if args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"); len(args) > 1 && args[1] == "serve" {
			pids = append(pids, pid)
		}
	}
	return pids
}
