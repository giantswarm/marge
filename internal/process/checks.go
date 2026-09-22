package process

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
)

// evaluateChecks decides whether the PR is green enough to merge. It
// returns true only when every required context reported success, no
// security check failed, and every remaining red check is a non-required
// check that is red on the base head too (pre-existing, named in the
// evidence). A pre-existing red check never shortens the wait for a
// pending or missing required context. Every other outcome is recorded on
// the entry and ends the PR. A protection the caller may not read leaves
// the required set empty; GitHub then enforces it at merge time and the
// refusal is classified as a wait.
func (p *Processor) evaluateChecks(ctx context.Context, run *prRun) bool {
	var deadline <-chan time.Time
	if p.CheckTimeout > 0 {
		deadline = time.After(p.CheckTimeout)
	}
	for {
		outcome, err := p.getCombinedCheckState(ctx, run.info)
		if err != nil {
			run.set(pr.StatusFailed, ghErrorDetail("check error", err))
			return false
		}
		switch outcome.state {
		case stateBlockedBudget:
			// CI never ran because a GitHub Actions budget / spending-limit
			// block prevented every job from starting. This is not a code
			// failure, so surface it under a distinct status and keep it out
			// of the rescue path.
			run.set(pr.StatusBlockedCI, blockedDetail(outcome.blockedChecks))
			return false
		case stateNoVerdict:
			// Every failing check established nothing about the code, so
			// there is nothing to rescue and a security check in this shape
			// is not a finding. The detail names the remedy per check.
			run.set(pr.StatusNoVerdict, noVerdictDetail(outcome.noVerdictChecks))
			return false
		}

		prot, err := p.requiredProtection(ctx, run.info, run.pull.GetBase().GetRef())
		if err != nil {
			run.set(pr.StatusFailed, ghErrorDetail("branch protection error", err))
			return false
		}
		required := evaluateRequired(prot.Contexts, outcome.reported)
		run.failing = outcome.failedChecks
		run.required = remedy.Required(required)
		run.statusTargets = outcome.statusTargets
		run.detailsURLs = outcome.detailsURLs
		run.reported = len(outcome.reported)
		run.pending = outcome.pendingChecks
		run.checksPending = outcome.state == statePending
		run.settledAt = outcome.settledAt

		if len(required.Failed) > 0 || outcome.state == stateFailure || outcome.state == stateError {
			if !p.classifyFailure(ctx, run, outcome, prot) {
				return false
			}
		}

		if required.allGreen() && outcome.state != statePending {
			return true
		}

		run.set(pr.StatusWaitingChecks, waitingDetail(required, outcome.state))
		p.recordSilentContext(run, required.Missing, time.Now())
		if deadline == nil {
			return false
		}
		select {
		case <-ctx.Done():
			run.set(pr.StatusSkipped, "cancelled")
			return false
		case <-deadline:
			return false
		case <-time.After(checkPollInterval):
		}
	}
}

func waitingDetail(required requiredOutcome, state string) string {
	var parts []string
	if len(required.Missing) > 0 {
		parts = append(parts, "required checks not reported: "+strings.Join(required.Missing, ", "))
	}
	if len(required.Pending) > 0 {
		parts = append(parts, "required checks pending: "+strings.Join(required.Pending, ", "))
	}
	if len(parts) == 0 {
		return "checks " + state
	}
	return strings.Join(parts, "; ")
}

// classifyFailure handles a head with at least one red check, in this
// order: CircleCI auto-cancel (no verdict), stale (fixed on the base since),
// obsolete (nobody has to fix it), security (never merge), then the
// required/non-required split. It returns
// true when every red check is non-required and red on the base head too;
// the caller still applies the required-check wait.
func (p *Processor) classifyFailure(ctx context.Context, run *prRun, outcome checkOutcome, prot protection) bool {
	if cancelled, note := p.classifyCancelled(ctx, run.pull, outcome); cancelled != nil {
		p.handleCancelled(ctx, run, cancelled)
		return false
	} else if note != "" {
		run.note(note)
	}
	// The staleness heuristic and the no-op rule both read the base...head
	// comparison, so it is fetched once for both. An error leaves it nil and
	// neither classification fires.
	cmp := p.compare(ctx, run.info, run.pull)
	if stale := p.classifyStale(ctx, run.info, run.pull, outcome.failedChecks, cmp); stale != nil {
		p.handleStale(ctx, run, stale)
		return false
	}
	if reason, detail := p.classifyObsolete(ctx, run.info, run.pull, cmp); reason != "" {
		run.markObsolete(reason, detail)
		return false
	}
	if name := classifySecurityFailure(outcome.failedChecks, p.securityPatterns()); name != "" {
		run.set(pr.StatusFailedSecurity, fmt.Sprintf("security check failed: %s", name))
		p.postOnce(ctx, run, pr.MarkerKindEvidence, "security-blocked", "security check failed: "+name)
		return false
	}

	var real []string
	for _, name := range outcome.failedChecks {
		if isRequired(prot.Contexts, name) {
			real = append(real, name)
			continue
		}
		if p.redOnBase(ctx, run, name) {
			run.preexisting = append(run.preexisting, name)
			continue
		}
		real = append(real, name)
	}
	if len(real) > 0 {
		run.set(pr.StatusFailed, failureDetail(real))
		return false
	}
	sort.Strings(run.preexisting)
	return true
}

// redOnBase reports whether the named check is red on the base head as
// well, which makes the PR's red check pre-existing rather than caused by
// the PR. A check that is green, pending or absent on the base is not
// pre-existing: absent is not green, and neither is it red.
func (p *Processor) redOnBase(ctx context.Context, run *prRun, name string) bool {
	baseSHA := run.pull.GetBase().GetSHA()
	if baseSHA == "" {
		return false
	}
	states, err := p.baseContextStates(ctx, run.info, baseSHA)
	if err != nil {
		return false
	}
	st, ok := states[name]
	return ok && st.failed && !st.success
}

// checkOutcome is the result of evaluating a PR's combined commit status and
// check runs. failedChecks holds genuine failures; the other two lists hold
// checks that report failure although they never produced a verdict on the
// code. They are tracked apart so none of them is reported as a real CI
// failure.
type checkOutcome struct {
	state string
	// sha is the commit the statuses and check runs belong to: the PR head
	// as GitHub resolved it for this poll.
	sha          string
	failedChecks []string
	// blockedChecks failed because a GitHub Actions budget block kept the
	// job from starting.
	blockedChecks []string
	// pendingChecks have not finished, with the message each reports. A
	// gate that waits on a job nobody started says so there, and says
	// nowhere else.
	pendingChecks []rules.PendingCheck
	// noVerdictChecks established nothing about the code: the job was
	// cancelled, or a project setting refused the pipeline.
	noVerdictChecks []noVerdictCheck
	// reported is the latest state of every context name on the head, the
	// input of the required-check guard. A check that produced no verdict
	// is neither green nor red there.
	reported map[string]contextState
	// statusTargets maps each failing commit-status context to its
	// target_url, so the CircleCI lookup can find the build behind it.
	// Check runs have no entry.
	statusTargets map[string]string
	// detailsURLs maps each failing check run to its details URL, which
	// carries the Actions job id the log excerpt is read from.
	detailsURLs map[string]string
	// settledAt is the newest completion time among the head's contexts.
	settledAt time.Time
}

func (p *Processor) getCombinedCheckState(ctx context.Context, info pr.PRInfo) (checkOutcome, error) {
	ref := fmt.Sprintf("refs/pull/%d/head", info.Number)
	combined, _, err := p.Client.Repositories.GetCombinedStatus(ctx, info.Owner, info.Repo, ref, &github.ListOptions{PerPage: 100})
	if err != nil {
		return checkOutcome{}, err
	}

	combinedState := combined.GetState()

	checkRuns, _, err := p.Client.Checks.ListCheckRunsForRef(ctx, info.Owner, info.Repo, ref, &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return checkOutcome{}, err
	}

	reported := make(map[string]contextState)
	record := func(name string, success, failed bool, at time.Time) {
		recordContext(reported, name, success, failed, at)
	}

	if checkRuns.GetTotal() == 0 && len(combined.Statuses) == 0 {
		return checkOutcome{state: stateSuccess, sha: combined.GetSHA(), reported: reported}, nil
	}

	var failedChecks []string
	var detailsURLs map[string]string
	var blockedChecks []string
	var pendingChecks []rules.PendingCheck
	var noVerdictChecks []noVerdictCheck
	allComplete := true
	hasFailure := false
	for _, cr := range checkRuns.CheckRuns {
		name := cr.GetName()
		if cr.GetStatus() != statusCompleted {
			allComplete = false
			if name != "" {
				pendingChecks = append(pendingChecks, rules.PendingCheck{Name: name, Output: checkOutputText(cr)})
			}
			record(name, false, false, time.Time{})
			continue
		}
		completed := cr.GetCompletedAt().Time
		conclusion := cr.GetConclusion()
		if isFailedConclusion(conclusion) {
			// A check run that reports failure although it never produced a
			// verdict on the code -- a budget block, a cancelled job, a
			// pipeline a project setting refuses -- belongs in its own
			// bucket, out of the failure counts and out of the rescue path.
			switch kind, reason := p.classifyCheckRun(ctx, info, cr, conclusion); kind {
			case kindBudgetBlock:
				if name != "" {
					blockedChecks = append(blockedChecks, name)
				}
				record(name, false, false, completed)
				continue
			case kindNoVerdict:
				if name != "" {
					noVerdictChecks = append(noVerdictChecks, noVerdictCheck{Name: name, Reason: reason})
				}
				record(name, false, false, completed)
				continue
			}
			hasFailure = true
			if name != "" {
				failedChecks = append(failedChecks, name)
				if url := cr.GetDetailsURL(); url != "" {
					if detailsURLs == nil {
						detailsURLs = make(map[string]string)
					}
					detailsURLs[name] = url
				}
			}
			record(name, false, true, completed)
			continue
		}
		// Neutral and skipped conclusions count as a pass for the guard
		// the way GitHub counts them for required checks.
		record(name, conclusion == stateSuccess || conclusion == "neutral" || conclusion == "skipped", false, completed)
	}

	var statusTargets map[string]string
	hasStatusFailure := false
	for _, s := range combined.Statuses {
		state := s.GetState()
		name := s.GetContext()
		updated := s.GetUpdatedAt().Time
		if state != stateFailure && state != stateError {
			record(name, state == stateSuccess, false, updated)
			continue
		}
		if name == "" {
			hasStatusFailure = true
			continue
		}
		// The CircleCI GitHub app reports the setup-workflow refusal in the
		// status description.
		if isSetupWorkflowBlock(s.GetDescription()) {
			noVerdictChecks = append(noVerdictChecks, noVerdictCheck{Name: name, Reason: circleCISetupReason})
			record(name, false, false, updated)
			continue
		}
		record(name, false, true, updated)
		hasStatusFailure = true
		failedChecks = append(failedChecks, name)
		if statusTargets == nil {
			statusTargets = make(map[string]string)
		}
		statusTargets[name] = s.GetTargetURL()
	}

	// The same check can arrive as a commit status and as a check run.
	noVerdictChecks = dedupeByName(noVerdictChecks)

	out := checkOutcome{
		sha:             combined.GetSHA(),
		failedChecks:    failedChecks,
		blockedChecks:   blockedChecks,
		pendingChecks:   pendingChecks,
		noVerdictChecks: noVerdictChecks,
		reported:        reported,
		statusTargets:   statusTargets,
		detailsURLs:     detailsURLs,
		settledAt:       newestReport(reported),
	}
	switch {
	case hasFailure || hasStatusFailure:
		out.state = stateFailure
	case !allComplete:
		out.state = statePending
	case len(noVerdictChecks) > 0:
		// Nothing genuinely failed and every failing check established
		// nothing about the code: the detail carries the remedy.
		out.state = stateNoVerdict
	case len(blockedChecks) > 0:
		// Every failing check was a budget block and nothing genuinely
		// failed: the PR's CI could not run at all.
		out.state = stateBlockedBudget
	case combinedState == stateFailure || combinedState == stateError:
		out.state = combinedState
	case combinedState == statePending && len(combined.Statuses) > 0:
		out.state = statePending
	default:
		out.state = stateSuccess
	}
	return out, nil
}

// checkOutputText joins what a check run reports about the head. The three
// fields are one message split for rendering, and a signal reads the
// message.
func checkOutputText(cr *github.CheckRun) string {
	out := cr.GetOutput()
	parts := make([]string, 0, 3)
	for _, part := range []string{out.GetTitle(), out.GetSummary(), out.GetText()} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

// checkKind says what a failing check run actually established.
type checkKind string

const (
	// kindRealFailure: the check ran and the failure is about the code.
	kindRealFailure checkKind = ""
	// kindBudgetBlock: an Actions budget block kept the job from starting.
	kindBudgetBlock checkKind = "budget-block"
	// kindNoVerdict: the check established nothing about the code.
	kindNoVerdict checkKind = "no-verdict"
)

// classifyCheckRun decides whether a failing check run produced a verdict on
// the code, and when it did not, why.
//
// The check run's own fields are read first, which costs no extra request.
// Only a run that carries annotations and still looks like a failure has
// them fetched, because GitHub records the budget-block message there. A
// fetch error is treated as kindRealFailure so a transient API error never
// hides a real failure.
func (p *Processor) classifyCheckRun(ctx context.Context, info pr.PRInfo, cr *github.CheckRun, conclusion string) (checkKind, string) {
	out := cr.GetOutput()
	title, summary, text := out.GetTitle(), out.GetSummary(), out.GetText()
	if kind, reason := classifyCheckRunMessages(title, summary, text, nil); kind != kindRealFailure {
		return kind, reason
	}
	// A cancelled job built, tested and scanned nothing, whatever it
	// reports: the conclusion alone settles it.
	if conclusion == conclusionCancelled {
		return kindNoVerdict, cancelledReason
	}
	if out.GetAnnotationsCount() == 0 {
		return kindRealFailure, ""
	}

	annotations, _, err := p.Client.Checks.ListCheckRunAnnotations(ctx, info.Owner, info.Repo, cr.GetID(), nil)
	if err != nil {
		return kindRealFailure, ""
	}
	messages := make([]string, 0, len(annotations))
	for _, a := range annotations {
		messages = append(messages, a.GetMessage())
	}
	return classifyCheckRunMessages("", "", "", messages)
}

// classifyCheckRunMessages decides what a set of check-run messages says
// about a failing run. It is pure so the classification can be exercised
// without hitting the GitHub API.
func classifyCheckRunMessages(title, summary, text string, annotationMessages []string) (checkKind, string) {
	switch {
	case isBudgetBlockOutput(title, summary, text, annotationMessages):
		return kindBudgetBlock, ""
	case matchesAnyMessage(isSetupWorkflowBlock, title, summary, text, annotationMessages):
		return kindNoVerdict, circleCISetupReason
	}
	return kindRealFailure, ""
}

// dedupeByName keeps the first entry for each check name, preserving order.
func dedupeByName(checks []noVerdictCheck) []noVerdictCheck {
	if len(checks) < 2 {
		return checks
	}
	seen := make(map[string]struct{}, len(checks))
	out := checks[:0]
	for _, c := range checks {
		if _, dup := seen[c.Name]; dup {
			continue
		}
		seen[c.Name] = struct{}{}
		out = append(out, c)
	}
	return out
}
