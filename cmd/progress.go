package cmd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// progressInterval is how often a run that has nothing new to report says
// it is still working. A gateway that resets its budget for one call on
// progress then never cuts a run that is waiting on GitHub, such as the
// rebase pass after a merge, which waits for minutes.
const progressInterval = 15 * time.Second

// runProgress reports a tool call's advance to the caller that asked for it
// with a progressToken. Every notification carries a progress value higher
// than the last one, as the specification requires, so the value counts
// notifications and not PRs; the message carries the PR counts. A nil
// *runProgress reports nothing.
type runProgress struct {
	send func(progress float64, message string)

	mu         sync.Mutex
	sent       int
	planned    int
	finished   int
	revisiting int
	closed     bool
}

// Planned adds n PRs to the ones the call will process. A call over several
// teams plans each team's PRs as it reaches them.
func (p *runProgress) Planned(n int) {
	p.update(func() { p.planned += n })
}

// Finished records one PR processed.
func (p *runProgress) Finished() {
	p.update(func() { p.finished++ })
}

// Revisiting records n PRs a merge of this call made dirty, which the call
// waits on before it processes them again.
func (p *runProgress) Revisiting(n int) {
	if n == 0 {
		return
	}
	p.update(func() { p.revisiting = n })
}

func (p *runProgress) update(change func()) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	change()
	p.reportLocked()
}

// heartbeat repeats the current state.
func (p *runProgress) heartbeat() {
	p.update(func() {})
}

// close stops every further report: a notification after the call's
// result would carry a token the caller has already let go of.
func (p *runProgress) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

func (p *runProgress) reportLocked() {
	if p.closed {
		return
	}
	p.sent++
	p.send(float64(p.sent), p.messageLocked())
}

func (p *runProgress) messageLocked() string {
	if p.planned == 0 {
		return "finding the PRs"
	}
	message := fmt.Sprintf("%d of %d PRs done", p.finished, p.planned)
	if p.revisiting > 0 {
		message += fmt.Sprintf("; waiting on %d PRs a merge of this run made dirty", p.revisiting)
	}
	return message
}

type progressKey struct{}

func withProgress(ctx context.Context, p *runProgress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
}

// progressFrom returns the call's reporter, or nil when the caller asked
// for no progress.
func progressFrom(ctx context.Context) *runProgress {
	p, _ := ctx.Value(progressKey{}).(*runProgress)
	return p
}

// reportProgress runs a tool handler with a reporter on its context when
// the call carries a progressToken, and with a heartbeat until it returns.
func reportProgress(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if request.Params.Meta == nil || request.Params.Meta.ProgressToken == nil {
			return next(ctx, request)
		}
		srv := server.ServerFromContext(ctx)
		if srv == nil {
			return next(ctx, request)
		}
		token := request.Params.Meta.ProgressToken
		progress := &runProgress{send: func(value float64, message string) {
			// A notification the transport could not take is dropped: the
			// next one says the same and more.
			_ = srv.SendNotificationToClient(ctx, string(mcp.MethodNotificationProgress), map[string]any{
				"progressToken": token,
				"progress":      value,
				"message":       message,
			})
		}}

		stop := progress.start(progressInterval)
		defer stop()
		return next(withProgress(ctx, progress), request)
	}
}

// start reports the current state now and every interval after, until stop
// returns. Nothing is reported after stop.
func (p *runProgress) start(interval time.Duration) (stop func()) {
	p.heartbeat()
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				p.heartbeat()
			}
		}
	}()
	return func() {
		close(quit)
		<-done
		p.close()
	}
}
