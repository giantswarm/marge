package process

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

func (p *Processor) approve(ctx context.Context, run *prRun) error {
	if p.ownPR(run) {
		return nil
	}
	reviews, _, err := p.Client.PullRequests.ListReviews(ctx, run.info.Owner, run.info.Repo, run.info.Number, nil)
	if err != nil {
		run.set(pr.StatusFailed, ghErrorDetail("review list error", err))
		return err
	}

	for _, r := range reviews {
		if r.GetUser().GetLogin() == p.Login && r.GetState() == "APPROVED" {
			return nil
		}
	}

	if err := p.ensureWriteAccess(ctx, run.info.Owner, run.info.Repo); err != nil {
		run.set(pr.StatusFailed, writeAccessDetail(err))
		return err
	}

	run.set(pr.StatusApproving, "")

	_, _, err = p.Client.PullRequests.CreateReview(ctx, run.info.Owner, run.info.Repo, run.info.Number, &github.PullRequestReviewRequest{
		Event: new("APPROVE"),
	})
	if err != nil {
		run.set(pr.StatusFailed, ghErrorDetail("approve error", err))
		return err
	}

	return nil
}

// ownPR reports whether the caller authored the PR, as a self-hosted
// Renovate running under the caller's account does. GitHub refuses an
// author's approval of their own PR, and a merge by the owner needs none.
func (p *Processor) ownPR(run *prRun) bool {
	return p.Login != "" && strings.EqualFold(run.pull.GetUser().GetLogin(), p.Login)
}

// isBaseBranchModified returns true when the merge failed because the base
// branch SHA changed (e.g. another PR was just merged into the same branch).
// These errors are retryable -- re-fetching the PR and retrying usually works.
func isBaseBranchModified(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "base branch was modified")
}

// isReviewRefusal reports whether GitHub refused the merge because a review
// requirement is unmet.
func isReviewRefusal(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "approving review") || strings.Contains(msg, "review is required") || strings.Contains(msg, "code owner")
}

// isChecksRefusal reports whether GitHub refused the merge because a
// required status check is not satisfied.
func isChecksRefusal(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "required status check") || strings.Contains(msg, "status checks")
}

func (p *Processor) mergeRetries() int {
	if p.MergeMaxRetries > 0 {
		return p.MergeMaxRetries
	}
	return mergeMaxRetries
}

func (p *Processor) mergeWait() time.Duration {
	if p.MergeRetryWait > 0 {
		return p.MergeRetryWait
	}
	return mergeRetryBaseWait
}

// mergeInLane merges the PR inside its repository's lane, MergeLane merges
// wide. The PRs of a repository are read together, so a sibling that merged
// after this PR was read left its mergeable state stale: the PR is read
// again before the merge decides on it, and decides as it would have,
// processed after that sibling.
func (p *Processor) mergeInLane(ctx context.Context, run *prRun) {
	lane, _ := p.mergeLanes.get(run.info.Owner+"/"+run.info.Repo, func() (Slots, error) {
		return NewSlots(p.MergeLane), nil
	})
	release, err := lane.acquire(ctx)
	if err != nil {
		run.set(pr.StatusSkipped, "cancelled")
		return
	}
	defer release()

	if p.mergesIn(run.info) != run.mergesSeen {
		checked := run.pull.GetHead().GetSHA()
		fresh, _, err := p.Client.PullRequests.Get(ctx, run.info.Owner, run.info.Repo, run.info.Number)
		if err != nil {
			run.set(pr.StatusFailed, ghErrorDetail("fetch error", err))
			return
		}
		switch {
		case fresh.GetMerged():
			run.set(pr.StatusAlreadyMerged, "")
			return
		case fresh.GetHead().GetSHA() != checked:
			run.set(pr.StatusWaitingChecks, headMovedDetail)
			return
		}
		run.pull = fresh
		if fresh.GetMergeableState() == "dirty" {
			p.setConflict(ctx, run, "merge conflict")
			return
		}
	}
	p.merge(ctx, run)
}

// headMovedDetail is a PR whose head moved after its checks were read: the
// checks that made it eligible say nothing about the new head, so the next
// sweep decides on it.
const headMovedDetail = "head moved since its checks were read; the next sweep decides"

// isHeadModified reports whether GitHub refused the merge because the head
// is no longer the commit the merge named.
func isHeadModified(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "head branch was modified")
}

// merge lands the PR with a squash. A PR behind its base is brought up to
// date instead and merges on a later sweep once its checks ran on the new
// head: that is the strict-protection chain, one round per sweep.
func (p *Processor) merge(ctx context.Context, run *prRun) {
	if run.pull.GetMergeableState() == "behind" {
		p.updateBranch(ctx, run, "behind base")
		return
	}

	maxRetries := p.mergeRetries()
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt == 1 {
			run.set(pr.StatusMerging, "")
		} else {
			run.set(pr.StatusRetrying, fmt.Sprintf("attempt %d/%d", attempt, maxRetries))
		}

		// The merge names the head its checks were read on, so a commit
		// pushed since is refused by GitHub instead of merged unchecked.
		_, _, err := p.Client.PullRequests.Merge(ctx, run.info.Owner, run.info.Repo, run.info.Number, "", &github.PullRequestOptions{
			MergeMethod: "squash",
			SHA:         run.pull.GetHead().GetSHA(),
		})
		if err == nil {
			run.set(pr.StatusMerged, mergedDetail(run))
			p.recordMerge(run.info)
			if len(run.preexisting) > 0 {
				p.postOnce(ctx, run, pr.MarkerKindEvidence, "merged-past-red-check", preexistingNote(run)+" (red on the base head too)")
			}
			return
		}

		if !isBaseBranchModified(err) {
			errMsg := err.Error()
			switch {
			case isReviewRefusal(err):
				run.set(pr.StatusAwaitingApproval, ghErrorDetail("merge refused", err))
				p.postOnce(ctx, run, pr.MarkerKindEvidence, "awaiting-approval", "the sweep's approval did not satisfy the review rule")
			case isHeadModified(err):
				run.set(pr.StatusWaitingChecks, headMovedDetail)
			case isChecksRefusal(err):
				run.set(pr.StatusWaitingChecks, ghErrorDetail("merge refused", err))
			case strings.Contains(errMsg, "409") || strings.Contains(errMsg, "conflict"):
				p.setConflict(ctx, run, "merge conflict")
			default:
				run.set(pr.StatusFailed, ghErrorDetail("merge error", err))
			}
			return
		}

		if attempt == maxRetries {
			run.set(pr.StatusFailed, fmt.Sprintf("base branch modified after %d attempts", maxRetries))
			return
		}

		wait := p.mergeWait() * time.Duration(attempt)
		select {
		case <-ctx.Done():
			run.set(pr.StatusSkipped, "cancelled")
			return
		case <-time.After(wait):
		}

		refreshed, _, fetchErr := p.Client.PullRequests.Get(ctx, run.info.Owner, run.info.Repo, run.info.Number)
		if fetchErr != nil {
			run.set(pr.StatusFailed, ghErrorDetail("retry fetch error", fetchErr))
			return
		}
		if refreshed.GetMerged() {
			run.set(pr.StatusAlreadyMerged, "merged between retries")
			return
		}
		if refreshed.GetMergeableState() == "dirty" {
			p.setConflict(ctx, run, "merge conflict on retry")
			return
		}
		if refreshed.GetMergeableState() == "behind" {
			run.pull = refreshed
			p.updateBranch(ctx, run, "fell behind while merging")
			return
		}
	}
}

func mergedDetail(run *prRun) string {
	return withNote("squash", preexistingNote(run))
}

// preexistingNote names the red non-required checks that are red on the
// base head too, or "" when there are none.
func preexistingNote(run *prRun) string {
	if len(run.preexisting) == 0 {
		return ""
	}
	return "pre-existing red checks: " + strings.Join(run.preexisting, ", ")
}
