package remedy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
)

// Default returns the vocabulary this build implements.
func Default() *Registry {
	return NewRegistry(
		updateBranch{},
		rerunFailed{},
		circleCIRetry{},
		closePR{},
		markWait{},
		dispatchAlignWorkflow{},
		fixProtectionContext{},
		strictChain{},
	)
}

// commonGuards are enforced by every action: the sweep touches a trusted
// bot's PR only, never one with a failing security check, and acts on a log
// excerpt rather than on a check name.
func commonGuards(name Name) []Guard {
	return []Guard{TrustedAuthor, NoSecurityFailure, LogMatched, OncePerChange(name)}
}

// updateBranch merges the base branch into the PR head. GitHub schedules the
// update and answers 202, which go-github surfaces as an AcceptedError: that
// is the success path.
type updateBranch struct{}

func (updateBranch) Name() Name { return UpdateBranch }

// A branch behind its base says nothing about the log, so this action asks
// for no excerpt. It also runs with a failing security check: it neither
// merges nor rescues, and a scan the base branch has already fixed is
// exactly what a refresh is for.
func (updateBranch) Guards() []Guard {
	return []Guard{TrustedAuthor, OncePerChange(UpdateBranch)}
}

func (updateBranch) Apply(ctx context.Context, req *Request) (Outcome, error) {
	_, _, err := req.Deps.GitHub.PullRequests.UpdateBranch(ctx, req.Info.Owner, req.Info.Repo, req.Info.Number, nil)
	var accepted *github.AcceptedError
	if err != nil && !errors.As(err, &accepted) {
		return Outcome{}, fmt.Errorf("update-branch: %w", err)
	}
	return Outcome{Applied: true, Detail: "branch updated from " + req.Pull.GetBase().GetRef()}, nil
}

// runURLRE reads the workflow run id out of a check run's details URL.
var runURLRE = regexp.MustCompile(`/actions/runs/(\d+)`)

// rerunFailed reruns the failed jobs of the GitHub Actions run behind the
// matched check, which also releases the jobs those failures blocked.
type rerunFailed struct{}

func (rerunFailed) Name() Name      { return RerunFailed }
func (rerunFailed) Guards() []Guard { return commonGuards(RerunFailed) }

func (rerunFailed) Apply(ctx context.Context, req *Request) (Outcome, error) {
	match := runURLRE.FindStringSubmatch(req.CheckURL)
	if match == nil {
		return Outcome{Refused: "no Actions run behind " + req.Check}, nil
	}
	runID, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return Outcome{Refused: "no Actions run behind " + req.Check}, nil
	}
	if _, err := req.Deps.GitHub.Actions.RerunFailedJobsByID(ctx, req.Info.Owner, req.Info.Repo, runID); err != nil {
		return Outcome{}, fmt.Errorf("rerun-failed: %w", err)
	}
	return Outcome{Applied: true, Detail: fmt.Sprintf("failed jobs of run %d rerun", runID)}, nil
}

// circleCIRetry reruns the CircleCI workflow the matched check belongs to,
// from its failed jobs, and falls back to the single-build retry when
// CircleCI has no failed job to rerun from.
type circleCIRetry struct{}

func (circleCIRetry) Name() Name      { return CircleCIRetry }
func (circleCIRetry) Guards() []Guard { return commonGuards(CircleCIRetry) }

func (circleCIRetry) Apply(ctx context.Context, req *Request) (Outcome, error) {
	client := req.Deps.CircleCI
	if client == nil {
		return Outcome{Refused: "no CircleCI token: the build cannot be retried"}, nil
	}
	ref, ok := circleci.ParseBuildURL(req.CheckURL)
	if !ok {
		return Outcome{Refused: "no CircleCI build behind " + req.Check}, nil
	}
	build, err := client.Build(ctx, ref)
	if err != nil {
		return Outcome{}, fmt.Errorf("circleci-retry: %w", err)
	}

	if build.Workflows.WorkflowID != "" {
		err := client.RerunWorkflowFromFailed(ctx, build.Workflows.WorkflowID)
		if err == nil {
			return Outcome{Applied: true, Detail: "workflow " + workflowLabel(build.Workflows) + " rerun from failed"}, nil
		}
		if !rerunRefused(err) {
			return Outcome{}, fmt.Errorf("circleci-retry: rerun of workflow %s: %w", workflowLabel(build.Workflows), err)
		}
	}
	retried, err := client.Retry(ctx, ref)
	if err != nil {
		return Outcome{}, fmt.Errorf("circleci-retry: retry of build %d: %w", ref.Num, err)
	}
	return Outcome{Applied: true, Detail: fmt.Sprintf("build %d retried as %d", ref.Num, retried.BuildNum)}, nil
}

// workflowLabel names a workflow for the evidence: its name when CircleCI
// reported one, its id otherwise.
func workflowLabel(w circleci.Workflow) string {
	if w.WorkflowName != "" {
		return w.WorkflowName
	}
	return w.WorkflowID
}

// rerunRefused reports whether CircleCI turned the workflow rerun down for a
// reason the single-build retry can still get past: no failed job to rerun
// from, or no such workflow.
func rerunRefused(err error) bool {
	var apiErr *circleci.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusNotFound
}
