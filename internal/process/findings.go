package process

import (
	"context"
	"strings"
	"time"

	"github.com/giantswarm/marge/internal/pr"
)

// recordMergeFinding records the base branch setting that stands between an
// eligible PR and its merge. The sweep keeps refusing the PR exactly as it
// did before; the finding only says why, so the fix goes to the repository.
//
// A PR carries one finding. A code-owner rule refuses the merge outright,
// and strict protection only delays it, so a repository with both reports
// the rule that holds the queue.
//
// A protection the caller may not read yields no finding: an error here
// must never change what the sweep does with the PR.
func (p *Processor) recordMergeFinding(ctx context.Context, run *prRun) {
	base := run.pull.GetBase().GetRef()

	if review, err := p.requiredReview(ctx, run.info, base); err == nil && review.CodeOwnerReviews {
		run.status.SetFinding(run.idx, &pr.RepoFinding{Cause: pr.FindingCodeOwnerReview})
		return
	}
	if prot, err := p.requiredProtection(ctx, run.info, base); err == nil && prot.Strict {
		run.status.SetFinding(run.idx, &pr.RepoFinding{Cause: pr.FindingStrictProtection})
	}
}

// recordSilentContext records a required context that reported nothing on a
// head whose other checks finished pr.SilentContextAfter ago. A context that
// may still report is a wait, so a PR with a pending check records nothing.
func (p *Processor) recordSilentContext(run *prRun, missing []string, now time.Time) {
	if len(missing) == 0 || run.checksPending {
		return
	}
	since := run.settledAt
	if since.IsZero() {
		since = run.info.CreatedAt
	}
	if since.IsZero() || now.Sub(since) < pr.SilentContextAfter {
		return
	}
	run.status.SetFinding(run.idx, &pr.RepoFinding{
		Cause:  pr.FindingSilentContext,
		Detail: strings.Join(missing, ", "),
	})
}
