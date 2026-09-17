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
	p.postMarker(ctx, run, &pr.RescueMarker{Kind: kind, Outcome: outcome, Reason: reason})
}

// postMarker writes marker unless an equivalent one already stands, and
// fills in the fields every marker carries: the tool, the head, the time and
// the fingerprint. The caller sets the kind, the outcome and whatever the
// outcome describes.
func (p *Processor) postMarker(ctx context.Context, run *prRun, marker *pr.RescueMarker) {
	if p.DryRun || !p.Actions.Has(ActionMark) {
		return
	}
	head := run.pull.GetHead().GetSHA()
	current := run.fingerprint(ctx, p)
	for _, existing := range run.markers(ctx, p) {
		if existing.Kind != marker.Kind || existing.Outcome != marker.Outcome {
			continue
		}
		existing.MarkStale(head, func() pr.Fingerprint { return current })
		if !existing.Stale {
			return
		}
	}

	marker.Tool = markerTool
	marker.HeadSHA = head
	marker.At = time.Now().UTC().Truncate(time.Second)
	marker.Fingerprint = current
	_, _, err := p.Client.Issues.CreateComment(ctx, run.info.Owner, run.info.Repo, run.info.Number, github.IssueCommentRequest{Body: marker.CommentBody()})
	if err != nil {
		run.note("marker not written: " + ghErrorDetail("", err))
		return
	}
	run.appendMarker(marker)
}
