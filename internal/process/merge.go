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

		_, _, err := p.Client.PullRequests.Merge(ctx, run.info.Owner, run.info.Repo, run.info.Number, "", &github.PullRequestOptions{
			MergeMethod: "squash",
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
			case isChecksRefusal(err):
				run.set(pr.StatusWaitingChecks, ghErrorDetail("merge refused", err))
			case strings.Contains(errMsg, "409") || strings.Contains(errMsg, "conflict"):
				p.setConflict(run, "merge conflict")
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
			p.setConflict(run, "merge conflict on retry")
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
