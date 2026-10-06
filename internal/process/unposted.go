package process

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
)

// circleContextPrefix starts every commit status a CircleCI job posts; the
// rest of the context is the job's name.
const circleContextPrefix = "ci/circleci: "

// unpostedAfter is how long a head stays settled before the sweep asks
// CircleCI about required contexts that never reported. CircleCI posts a
// job's status within seconds of the job ending; the margin keeps a slow
// post or a pipeline that has not started yet from being read as a lost one.
const unpostedAfter = 10 * time.Minute

// ciRerun is one rerun the sweep can ask a CI system for on the PR head,
// for a run that ended without giving the code a verdict. Each is made at
// most once per head, and the CI system's own record decides whether it
// was: a CircleCI workflow run tagged as a rerun, an Actions run past its
// first attempt. No state lives in the sweep.
type ciRerun struct {
	// What names the run for status output, e.g. "workflow build
	// (pipeline 483)".
	What string
	// Why names the shape that calls for the rerun, e.g. "statuses not
	// posted"; the status detail carries it.
	Why string
	// Done is true when the CI system already records a rerun on this head.
	Done bool
	// Blocked says why the rerun cannot be made (no token); empty when it
	// can.
	Blocked string
	// Run asks the CI system for the rerun.
	Run func(context.Context) error
}

// rerunOnce makes the reruns unless a rule stands in the way, and records
// the outcome on the entry. waiting is what the PR waits for; it leads every
// detail. A rerun already made on this head, or one the sweep may not make,
// leaves the PR without a verdict rather than waiting: nothing will report
// by itself.
func (p *Processor) rerunOnce(ctx context.Context, run *prRun, waiting string, reruns []ciRerun) {
	whats := make([]string, len(reruns))
	for i, r := range reruns {
		whats[i] = r.What
	}
	why := reruns[0].Why
	subject := strings.Join(whats, ", ")

	if slices.ContainsFunc(reruns, func(r ciRerun) bool { return r.Done }) {
		run.set(pr.StatusNoVerdict, fmt.Sprintf("%s; %s rerun once on this head, %s again", waiting, subject, why))
		return
	}
	if !p.Actions.Has(ActionRetry) || p.DryRun {
		run.set(pr.StatusNoVerdict, fmt.Sprintf("%s; %s finished, %s; rerun needed", waiting, subject, why))
		return
	}
	for _, r := range reruns {
		if r.Blocked != "" {
			run.set(pr.StatusNoVerdict, fmt.Sprintf("%s; %s finished, %s; rerun skipped: %s", waiting, subject, why, r.Blocked))
			return
		}
	}
	for _, r := range reruns {
		if err := r.Run(ctx); err != nil {
			run.set(pr.StatusNoVerdict, fmt.Sprintf("%s; %s finished, %s; rerun of %s failed: %v", waiting, subject, why, r.What, err))
			return
		}
	}
	detail := fmt.Sprintf("%s rerun (%s)", subject, why)
	run.set(pr.StatusRetried, "re-checking; "+detail)
	p.postOnce(ctx, run, pr.MarkerKindEvidence, "retry", detail)
}

// handleUnposted looks behind required CircleCI contexts that never
// reported on a settled head. CircleCI can run every job of a pipeline and
// post none of their statuses; the PR would then wait for ever. When the
// head's newest pipeline finished and its jobs carry the missing names, the
// workflows that ran them are rerun once per head. A head CircleCI never
// built is reported as such. It returns true when it decided the entry; on
// false the caller records the wait.
func (p *Processor) handleUnposted(ctx context.Context, run *prRun, required requiredOutcome, waiting string, now time.Time) bool {
	if p.CircleCI == nil || run.checksPending || len(required.Pending) > 0 {
		return false
	}
	missing := circleJobs(required.Missing)
	if len(missing) == 0 {
		return false
	}
	since := run.settledAt
	if since.IsZero() {
		since = run.info.CreatedAt
	}
	if since.IsZero() || now.Sub(since) < unpostedAfter {
		return false
	}

	head := run.pull.GetHead().GetSHA()
	pipelines, err := p.CircleCI.RevisionPipelines(ctx, run.info.Owner, run.info.Repo, circleBranch(run), head)
	if err != nil {
		run.note("CircleCI pipelines could not be read: " + err.Error())
		return false
	}
	if len(pipelines) == 0 {
		run.set(pr.StatusNoVerdict, fmt.Sprintf("%s; CircleCI has no pipeline for head %s, push a commit to start one", waiting, shortSHA(head)))
		return true
	}
	pipeline := pipelines[0]
	workflows, err := p.CircleCI.PipelineWorkflows(ctx, pipeline.ID)
	if err != nil {
		run.note(fmt.Sprintf("CircleCI pipeline %d could not be read: %v", pipeline.Number, err))
		return false
	}

	blocked := ""
	if !p.CircleCI.HasToken() {
		blocked = "no CircleCI token configured"
	}
	var reruns []ciRerun
	seen := make(map[string]bool, len(workflows))
	for _, wf := range workflows {
		if !wf.Finished() {
			return false
		}
		if seen[wf.Name] {
			continue
		}
		// Newest first: the first run of a name is the one to rerun, and any
		// run of that name tagged as a rerun means one was made.
		seen[wf.Name] = true
		jobs, err := p.CircleCI.WorkflowJobs(ctx, wf.ID)
		if err != nil {
			run.note(fmt.Sprintf("CircleCI workflow %s could not be read: %v", wf.Name, err))
			return false
		}
		if !slices.ContainsFunc(jobs, func(j circleci.WorkflowJob) bool { return slices.Contains(missing, j.Name) }) {
			continue
		}
		id := wf.ID
		reruns = append(reruns, ciRerun{
			What:    fmt.Sprintf("workflow %s (pipeline %d)", wf.Name, pipeline.Number),
			Why:     "statuses not posted",
			Done:    slices.ContainsFunc(workflows, func(w circleci.PipelineWorkflow) bool { return w.Name == wf.Name && w.IsRerun() }),
			Blocked: blocked,
			Run:     func(ctx context.Context) error { return p.CircleCI.RerunWorkflow(ctx, id) },
		})
	}
	if len(reruns) == 0 {
		// No job of the pipeline carries the missing names: the protection
		// requires a context no job posts, which the silent-context finding
		// reports.
		return false
	}
	p.rerunOnce(ctx, run, waiting, reruns)
	return true
}

// circleJobs returns the job names behind the CircleCI contexts of names.
func circleJobs(names []string) []string {
	var jobs []string
	for _, n := range names {
		if job, ok := strings.CutPrefix(n, circleContextPrefix); ok {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// circleBranch is the branch CircleCI files the PR head's pipelines under:
// the head branch for a PR from the repository itself, pull/<n> for one
// from a fork.
func circleBranch(run *prRun) string {
	head := run.pull.GetHead()
	if head.GetRepo().GetFullName() != "" && head.GetRepo().GetFullName() != run.info.Owner+"/"+run.info.Repo {
		return fmt.Sprintf("pull/%d", run.info.Number)
	}
	return head.GetRef()
}
