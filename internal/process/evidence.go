package process

import (
	"context"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// markerTool is the tool name every marker written by the sweep carries.
const markerTool = "marge"

// postOnce writes a marker comment unless an equivalent one already stands
// for the current change: same kind and outcome, and either the same head
// or a matching fingerprint. Renovate force-pushes on every rebase, so the
// fingerprint, not the SHA, decides whether evidence is still current.
func (p *Processor) postOnce(ctx context.Context, run *prRun, kind, outcome, reason string) {
	if p.DryRun || !p.Actions.Has(ActionMark) {
		return
	}
	head := run.pull.GetHead().GetSHA()
	current := run.fingerprint(ctx, p)
	for _, existing := range run.markers(ctx, p) {
		if existing.Kind != kind || existing.Outcome != outcome {
			continue
		}
		existing.MarkStale(head, func() pr.Fingerprint { return current })
		if !existing.Stale {
			return
		}
	}

	marker := &pr.RescueMarker{
		Tool:        markerTool,
		Kind:        kind,
		Outcome:     outcome,
		Reason:      reason,
		HeadSHA:     head,
		At:          time.Now().UTC().Truncate(time.Second),
		Fingerprint: current,
	}
	_, _, err := p.Client.Issues.CreateComment(ctx, run.info.Owner, run.info.Repo, run.info.Number, github.IssueCommentRequest{Body: marker.CommentBody()})
	if err != nil {
		run.note("marker not written: " + ghErrorDetail("", err))
		return
	}
	run.appendMarker(marker)
}
