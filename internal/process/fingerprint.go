package process

import (
	"context"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/google/go-github/v91/github"
)

// FingerprintPR computes the content fingerprint of a PR at its current
// head: the title-derived change ID always, plus the patch ID from the
// compare diff (base...head) when GitHub delivers it in full. Fetch errors
// and truncated diffs leave PatchID empty and never fail the caller -- a
// fingerprint is corroborating evidence, not a verdict, and pr.Fingerprint
// falls back to the change ID on its own.
func FingerprintPR(ctx context.Context, client *github.Client, owner, repo string, pullReq *github.PullRequest) pr.Fingerprint {
	fp := pr.Fingerprint{ChangeID: pr.ChangeID(pullReq.GetTitle())}
	base, head := pullReq.GetBase().GetRef(), pullReq.GetHead().GetSHA()
	if base == "" || head == "" {
		return fp
	}
	// per_page bounds the commit list only; the files are always returned.
	cmp, _, err := client.Repositories.CompareCommits(ctx, owner, repo, base, head, &github.ListOptions{PerPage: 1})
	if err != nil {
		return fp
	}
	fp.PatchID = pr.PatchID(cmp.Files, pullReq.GetChangedFiles())
	return fp
}

// markStale sets the marker's Stale and Rebased flags against the PR's
// current head. The fingerprint is computed lazily: only a marker whose head
// moved and that carries a fingerprint costs a compare request.
func (p *Processor) markStale(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest, marker *pr.RescueMarker) {
	marker.MarkStale(pullReq.GetHead().GetSHA(), func() pr.Fingerprint {
		return FingerprintPR(ctx, p.Client, info.Owner, info.Repo, pullReq)
	})
}
