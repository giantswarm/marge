package remedy

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v92/github"
)

// strictChain lands a PR on a branch whose protection requires the head to
// be up to date. One sweep performs one round: a PR behind its base is
// updated and merges on a later sweep once its checks ran on the new head.
//
// It performs no protection write. The runbook lifts enforce_admins because
// a person cannot approve their own fix PR; the sweep only ever touches a
// bot's PR, where its own approving review satisfies the review rule. A
// merge GitHub still refuses for a review reason is reported, never forced.
type strictChain struct{}

func (strictChain) Name() Name { return StrictChain }

func (strictChain) Guards() []Guard {
	return []Guard{TrustedAuthor, NoSecurityFailure, RequiredChecksGreen, OncePerChange(StrictChain)}
}

func (strictChain) Apply(ctx context.Context, req *Request) (Outcome, error) {
	if state := req.Pull.GetState(); state != "open" {
		return Outcome{Refused: "the PR is " + state}, nil
	}
	if len(req.Failing) > 0 {
		return Outcome{Refused: "checks still failing: " + strings.Join(req.Failing, ", ")}, nil
	}

	switch req.Pull.GetMergeableState() {
	case "dirty":
		return Outcome{Refused: "merge conflict: the bot rebases it"}, nil
	case "behind":
		if reason := refuse(updateBranch{}.Guards(), req); reason != "" {
			return Outcome{Refused: reason}, nil
		}
		out, err := (updateBranch{}).Apply(ctx, req)
		if err != nil {
			return out, err
		}
		out.Detail = "behind base: " + out.Detail + "; merges on a later sweep"
		out.KeepClassification = true
		return out, nil
	}

	if err := approveOnce(ctx, req); err != nil {
		return Outcome{}, err
	}

	_, _, err := req.Deps.GitHub.PullRequests.Merge(ctx, req.Info.Owner, req.Info.Repo, req.Info.Number, "",
		&github.PullRequestOptions{MergeMethod: "squash"})
	if err != nil {
		return Outcome{Refused: "merge refused: " + err.Error()}, nil
	}
	return Outcome{Applied: true, Detail: "squashed and merged"}, nil
}

// approveOnce submits an approving review unless the sweep already has one
// standing on the PR. A bot PR needs it: the review rule is what the
// protection enforces, and an approval is what satisfies it.
func approveOnce(ctx context.Context, req *Request) error {
	reviews, _, err := req.Deps.GitHub.PullRequests.ListReviews(ctx, req.Info.Owner, req.Info.Repo, req.Info.Number, nil)
	if err != nil {
		return fmt.Errorf("strict-chain: listing reviews: %w", err)
	}
	for _, review := range reviews {
		if review.GetUser().GetLogin() == req.Deps.Login && review.GetState() == "APPROVED" {
			return nil
		}
	}
	_, _, err = req.Deps.GitHub.PullRequests.CreateReview(ctx, req.Info.Owner, req.Info.Repo, req.Info.Number,
		&github.PullRequestReviewRequest{Event: new("APPROVE")})
	if err != nil {
		return fmt.Errorf("strict-chain: approving: %w", err)
	}
	return nil
}
