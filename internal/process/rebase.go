package process

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/giantswarm/marge/internal/pr"
)

const (
	// defaultRebaseWait bounds the second pass of one sweep. Renovate
	// rebases a conflicted PR within about a minute of the merge that
	// conflicted it.
	defaultRebaseWait = 3 * time.Minute
	// rebasePollInterval is how often the second pass looks at one PR.
	rebasePollInterval = 15 * time.Second
)

// rebaseQueue holds what one sweep merged and which PRs that merge
// conflicted. A repository's PRs are processed one at a time, but several
// repositories run at once, so every field is taken under the mutex.
type rebaseQueue struct {
	rebaseMu sync.Mutex
	// mergedBy maps owner/repo to the first PR the sweep merged there.
	mergedBy map[string]int
	deferred []deferredPR
	// revisiting reports that the second pass is running. A PR the second
	// pass finds dirty is classified, never queued again.
	revisiting bool
}

// deferredPR is a PR the sweep's own merge made dirty, kept for the second
// pass.
type deferredPR struct {
	info    pr.PRInfo
	status  *pr.PRStatus
	idx     int
	sibling int
}

// recordMerge remembers that the sweep merged a PR of this repository.
func (p *Processor) recordMerge(info pr.PRInfo) {
	p.rebaseMu.Lock()
	defer p.rebaseMu.Unlock()
	if p.mergedBy == nil {
		p.mergedBy = make(map[string]int)
	}
	key := info.Owner + "/" + info.Repo
	if _, seen := p.mergedBy[key]; !seen {
		p.mergedBy[key] = info.Number
	}
}

// setConflict records a merge conflict. A conflict the sweep caused itself,
// by merging another PR of the same repository earlier in the run, is not
// work for a person: the bot rebases the PR and the second pass merges it.
func (p *Processor) setConflict(run *prRun, detail string) {
	p.rebaseMu.Lock()
	sibling, caused := p.mergedBy[run.info.Owner+"/"+run.info.Repo]
	queue := caused && !p.revisiting
	if queue {
		p.deferred = append(p.deferred, deferredPR{
			info: run.info, status: run.status, idx: run.idx, sibling: sibling,
		})
	}
	p.rebaseMu.Unlock()

	if !caused {
		run.set(pr.StatusConflict, detail)
		return
	}
	run.set(pr.StatusAwaitingRebase, fmt.Sprintf("conflicted by #%d, merged in this run; the bot rebases it", sibling))
}

// rebaseWait is how long the second pass waits for the bot.
func (p *Processor) rebaseWait() time.Duration {
	if p.RebaseWait > 0 {
		return p.RebaseWait
	}
	return defaultRebaseWait
}

// Revisit is the second pass of a sweep. It takes the PRs the sweep's own
// merges made dirty, waits for the bot to rebase them, and processes each
// one again. Without it such a PR waits for the next sweep, which is a day
// later, and every further PR of the repository waits behind it.
//
// It runs after the first pass, so the rest of the sweep already paid part
// of the wait. RebaseWait bounds the whole pass, not each PR: the wait is
// for one bot to catch up, and every deferred PR waits on the same bot. A PR
// the bot has not rebased when the wait runs out keeps its awaiting-rebase
// classification, and the next sweep decides.
func (p *Processor) Revisit(ctx context.Context, status *pr.PRStatus) {
	p.rebaseMu.Lock()
	pending := p.deferred
	p.deferred = nil
	p.revisiting = true
	p.rebaseMu.Unlock()

	deadline := time.Now().Add(p.rebaseWait())
	for _, d := range pending {
		p.revisitOne(ctx, d, deadline)
	}
}

// revisitOne waits until the PR is no longer dirty, then processes it again.
func (p *Processor) revisitOne(ctx context.Context, d deferredPR, deadline time.Time) {
	for {
		pullReq, _, err := p.Client.PullRequests.Get(ctx, d.info.Owner, d.info.Repo, d.info.Number)
		if err != nil {
			d.status.Update(d.idx, pr.StatusAwaitingRebase, ghErrorDetail("rebase check failed", err))
			return
		}
		if state := pullReq.GetMergeableState(); state != "dirty" && state != "unknown" {
			p.ProcessPR(ctx, d.info, d.status, d.idx)
			return
		}
		wait := min(rebasePollInterval, time.Until(deadline))
		if wait <= 0 {
			d.status.Update(d.idx, pr.StatusAwaitingRebase,
				fmt.Sprintf("conflicted by #%d, merged in this run; not rebased yet", d.sibling))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
