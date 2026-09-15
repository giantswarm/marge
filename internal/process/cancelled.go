package process

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
)

// cancelledResult describes why a failing PR was classified as cancelled
// rather than failed: every failing check is a CircleCI build that CircleCI
// itself cancelled instead of one that failed a step.
type cancelledResult struct {
	// HeadSHA is the commit whose statuses were read: the PR head as GitHub
	// resolved it for this poll.
	HeadSHA string
	// Builds lists the cancelled builds, one per failing check, in check
	// name order.
	Builds []cancelledBuild
}

// cancelledBuild is one auto-cancelled CircleCI build behind a failing
// check.
type cancelledBuild struct {
	Context  string
	Ref      circleci.BuildRef
	Revision string
	// WorkflowID is the workflow run the build belongs to, empty for a
	// build that runs outside a workflow.
	WorkflowID   string
	WorkflowName string
}

// onHead reports whether every cancelled build ran on the PR's current
// head. Only then is a retry meaningful: a build behind a newer head has
// been superseded by that head's own build.
func (r *cancelledResult) onHead() bool {
	for _, b := range r.Builds {
		if b.Revision != r.HeadSHA {
			return false
		}
	}
	return len(r.Builds) > 0
}

// detail renders the cancelled classification for status output, e.g.
// "build 1263 auto-cancelled; retry needed" or, behind a newer head,
// "build 1244 (2c0ce64) auto-cancelled; head is now 605a2d6, its build is
// the verdict".
func (r *cancelledResult) detail() string {
	if r.onHead() {
		nums := make([]string, len(r.Builds))
		for i, b := range r.Builds {
			nums[i] = strconv.Itoa(b.Ref.Num)
		}
		label := "build"
		if len(nums) > 1 {
			label = "builds"
		}
		return fmt.Sprintf("%s %s auto-cancelled; retry needed", label, strings.Join(nums, ", "))
	}
	parts := make([]string, len(r.Builds))
	for i, b := range r.Builds {
		parts[i] = fmt.Sprintf("build %d (%s)", b.Ref.Num, shortSHA(b.Revision))
	}
	return fmt.Sprintf("%s auto-cancelled; head is now %s, its build is the verdict",
		strings.Join(parts, ", "), shortSHA(r.HeadSHA))
}

// shortSHA abbreviates a commit SHA the way git does in status output.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// classifyCancelled decides whether a PR's check failure is a CircleCI
// auto-cancel rather than a real failure. It returns nil (keep the failed
// classification) unless every failing check is a commit status whose
// target_url points at a CircleCI build, and every one of those builds was
// cancelled rather than failed (see circleci.Build.AutoCancelled). A failing
// check run, a status from another CI system or a CircleCI build with a
// failed step keeps the failure real.
//
// The lookup is lazy: nothing is fetched unless a failing status points at
// CircleCI, so sweeps without CircleCI failures make no extra calls. When a
// build cannot be inspected (a private project without a token, an API
// error) the failure stays a failure and the returned note tells the
// operator why the build was not classified.
func (p *Processor) classifyCancelled(ctx context.Context, pullReq *github.PullRequest, outcome checkOutcome) (*cancelledResult, string) {
	if p.CircleCI == nil || len(outcome.failedChecks) == 0 {
		return nil, ""
	}
	head := outcome.sha
	if head == "" {
		head = pullReq.GetHead().GetSHA()
	}

	res := &cancelledResult{HeadSHA: head}
	var notes []string
	seen := make(map[string]bool, len(outcome.failedChecks))
	for _, name := range outcome.failedChecks {
		if seen[name] {
			continue
		}
		seen[name] = true
		ref, ok := circleci.ParseBuildURL(outcome.statusTargets[name])
		if !ok {
			// Not a CircleCI build: the failure is real.
			return nil, ""
		}
		build, err := p.CircleCI.Build(ctx, ref)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: build %d could not be inspected (%v)", name, ref.Num, err))
			continue
		}
		if !build.AutoCancelled() {
			return nil, ""
		}
		res.Builds = append(res.Builds, cancelledBuild{
			Context:      name,
			Ref:          ref,
			Revision:     build.VCSRevision,
			WorkflowID:   build.Workflows.WorkflowID,
			WorkflowName: build.Workflows.WorkflowName,
		})
	}
	if len(notes) > 0 {
		return nil, strings.Join(notes, "; ")
	}
	if len(res.Builds) == 0 {
		return nil, ""
	}
	sort.Slice(res.Builds, func(i, j int) bool { return res.Builds[i].Context < res.Builds[j].Context })
	return res, ""
}

// handleCancelled records the cancelled classification and, when the retry
// action is selected, this is not a dry run and the cancelled builds ran on
// the current head, asks CircleCI to run them again on the same commit so
// the PR gets a real verdict. A cancelled build behind a newer head is never
// retried: the new head's own build is the verdict, and the next sweep
// reads it.
//
// Each distinct workflow is rerun once, however many of its jobs were
// cancelled: the rerun covers them all, and a second one would restart the
// first job.
func (p *Processor) handleCancelled(ctx context.Context, run *prRun, res *cancelledResult) {
	detail := res.detail()
	run.set(pr.StatusCancelled, detail)

	if !p.Actions.Has(ActionRetry) || p.DryRun || !res.onHead() {
		return
	}
	if !p.CircleCI.HasToken() {
		run.set(pr.StatusCancelled, detail+"; retry skipped: no CircleCI token configured")
		return
	}

	reruns := make([]string, 0, len(res.Builds))
	rerun := make(map[string]bool, len(res.Builds))
	for _, b := range res.Builds {
		if b.WorkflowID != "" {
			if rerun[b.WorkflowID] {
				continue
			}
			rerun[b.WorkflowID] = true
		}
		msg, err := p.rerunOrRetry(ctx, b)
		if err != nil {
			// Keep the reruns that already succeeded visible so the
			// operator knows what is running.
			if len(reruns) > 0 {
				detail += "; " + strings.Join(reruns, ", ")
			}
			run.set(pr.StatusCancelled, detail+"; "+err.Error())
			return
		}
		reruns = append(reruns, msg)
	}
	run.set(pr.StatusRetried, "re-checking; "+strings.Join(reruns, ", "))
	p.postOnce(ctx, run, pr.MarkerKindEvidence, "retry", strings.Join(reruns, ", "))
}

// rerunOrRetry runs one cancelled build again and describes what happened
// in the words the status output uses.
//
// A build inside a workflow goes through the v2 workflow rerun, which also
// releases the jobs the cancel left blocked or not run. Without them a
// repository whose branch protection requires those downstream contexts
// never becomes mergeable. Two cases have no failed job for CircleCI to
// rerun from -- a build outside a workflow, and a workflow cancelled before
// any job failed -- and both fall back to the v1.1 single-build retry.
func (p *Processor) rerunOrRetry(ctx context.Context, b cancelledBuild) (string, error) {
	if b.WorkflowID != "" {
		err := p.CircleCI.RerunWorkflowFromFailed(ctx, b.WorkflowID)
		if err == nil {
			return fmt.Sprintf("workflow %s rerun from failed", workflowLabel(b)), nil
		}
		if !rerunRefused(err) {
			return "", fmt.Errorf("rerun of workflow %s failed: %w", workflowLabel(b), err)
		}
	}
	nb, err := p.CircleCI.Retry(ctx, b.Ref)
	if err != nil {
		return "", fmt.Errorf("retry of build %d failed: %w", b.Ref.Num, err)
	}
	return fmt.Sprintf("build %d retried as %d", b.Ref.Num, nb.BuildNum), nil
}

// rerunRefused reports whether CircleCI turned the workflow rerun down for
// a reason the single-build retry can still get past: no failed job to
// rerun from (400), or no such workflow (404). An authentication failure or
// a server error is not one of those, and the retry would meet the same
// wall.
func rerunRefused(err error) bool {
	var apiErr *circleci.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusNotFound
}

// workflowLabel names a workflow for status output: its name when CircleCI
// reported one, its id otherwise.
func workflowLabel(b cancelledBuild) string {
	if b.WorkflowName != "" {
		return b.WorkflowName
	}
	return b.WorkflowID
}
