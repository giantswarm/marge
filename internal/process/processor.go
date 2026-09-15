package process

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
)

const (
	checkPollInterval = 15 * time.Second

	mergeMaxRetries    = 3
	mergeRetryBaseWait = 10 * time.Second
)

// Processor sweeps one PR at a time. Only PRs authored by one of the four
// trusted bots (see pr.KindOf) are touched; there is no way to widen that
// set, and the caller's own PRs are not in it.
type Processor struct {
	Client         *github.Client
	DryRun         bool
	MergeAutoMerge bool
	Login          string

	// SecurityCheckPatterns are appended to DefaultSecurityCheckPatterns.
	// The built-in list can be widened, never narrowed.
	SecurityCheckPatterns []string

	// MergeMaxRetries is the maximum number of merge attempts when the base
	// branch is modified between fetch and merge. Zero uses the default (3).
	MergeMaxRetries int
	// MergeRetryWait is the base wait duration between merge retries.
	// Zero uses the default (10s). Actual wait = base * attempt number.
	MergeRetryWait time.Duration

	// Actions selects the steps this sweep performs. Nil performs all.
	Actions ActionSet

	// Policies is the resolved bot PR sweep policy of the scope, read from
	// the policy files before the sweep starts. Nil applies the company
	// defaults of pr.CompanyDefaults.
	Policies *policy.Set

	// CheckTimeout bounds how long the sweep waits for pending checks on
	// one PR. Zero means no wait: a PR with pending or missing required
	// checks is reported as waiting and the next sweep decides.
	CheckTimeout time.Duration

	// CircleCI looks behind failing "ci/circleci: <job>" commit statuses to
	// tell an auto-cancelled build from a real failure (see
	// classifyCancelled). Nil disables the lookup and every CircleCI
	// failure is taken at face value.
	CircleCI *circleci.Client

	// SupersededBy maps a PR to the sibling that carries a higher version of
	// the same dependency (see pr.FindSuperseded). It is computed once from
	// the sweep's PR list before processing starts and only read afterwards,
	// so it needs no lock. A nil map supersedes nothing.
	SupersededBy pr.SupersededBy

	staleCache
	accessCache
	protectionCache
	labelCache
}

func NewProcessor(client *github.Client, dryRun bool, mergeAutoMerge bool, login string) *Processor {
	return &Processor{
		Client:         client,
		DryRun:         dryRun,
		MergeAutoMerge: mergeAutoMerge,
		Login:          login,
	}
}

// prRun carries one PR through a sweep: what was fetched about it, the
// status row it reports to, and the comments read once for every consumer
// (rescue markers, evidence).
type prRun struct {
	info   pr.PRInfo
	pull   *github.PullRequest
	status *pr.PRStatus
	idx    int

	commentsLoaded bool
	commentMarkers []*pr.RescueMarker
	fingerprintSet bool
	fp             pr.Fingerprint
	notes          []string
	// preexisting names the red non-required checks the PR merged past
	// because they are red on the base head too.
	preexisting []string
	// untouched holds a PR of a repository whose policy switched the sweep
	// off. Such a repository receives no write at all, the classification
	// label included.
	untouched bool
}

func (r *prRun) set(state pr.StatusState, detail string) {
	r.status.Update(r.idx, state, detail)
}

// markObsolete records the entry as obsolete together with the reason, so a
// consumer dispatches on the reason instead of parsing the detail.
func (r *prRun) markObsolete(reason pr.ObsoleteReason, detail string) {
	r.status.MarkObsolete(r.idx, reason, detail)
}

// note records an operational remark shown after the detail, for instance
// a label that could not be written.
func (r *prRun) note(s string) {
	r.notes = append(r.notes, s)
}

// markers returns every ai-rescue marker on the PR, oldest first, loading
// the comments on first use. A listing error yields no markers; a
// comment problem must never change a sweep result.
func (r *prRun) markers(ctx context.Context, p *Processor) []*pr.RescueMarker {
	if r.commentsLoaded {
		return r.commentMarkers
	}
	r.commentsLoaded = true
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := p.Client.Issues.ListComments(ctx, r.info.Owner, r.info.Repo, r.info.Number, opts)
		if err != nil {
			return r.commentMarkers
		}
		for _, c := range comments {
			if m := pr.ParseRescueMarker(c.GetBody()); m != nil {
				r.commentMarkers = append(r.commentMarkers, m)
			}
		}
		if resp.NextPage == 0 {
			return r.commentMarkers
		}
		opts.Page = resp.NextPage
	}
}

func (r *prRun) appendMarker(m *pr.RescueMarker) {
	r.commentMarkers = append(r.commentMarkers, m)
}

// rescueMarker returns the newest rescue-attempt marker, ignoring sweep
// evidence, or nil.
func (r *prRun) rescueMarker(ctx context.Context, p *Processor) *pr.RescueMarker {
	var newest *pr.RescueMarker
	for _, m := range r.markers(ctx, p) {
		if !m.IsEvidence() {
			newest = m
		}
	}
	return newest
}

func (r *prRun) fingerprint(ctx context.Context, p *Processor) pr.Fingerprint {
	if !r.fingerprintSet {
		r.fingerprintSet = true
		r.fp = FingerprintPR(ctx, p.Client, r.info.Owner, r.info.Repo, r.pull)
	}
	return r.fp
}

func (p *Processor) ProcessPR(ctx context.Context, info pr.PRInfo, status *pr.PRStatus, idx int) {
	pullReq, _, err := p.Client.PullRequests.Get(ctx, info.Owner, info.Repo, info.Number)
	if err != nil {
		status.Update(idx, pr.StatusFailed, ghErrorDetail("fetch error", err))
		return
	}
	run := &prRun{info: info, pull: pullReq, status: status, idx: idx}
	defer p.finish(ctx, run)

	resolved := p.Policies.For(info.Repo)
	status.SetPolicy(idx, resolved)
	if !resolved.Sweep {
		run.untouched = true
		run.set(pr.StatusSkipped, "sweep switched off for this repository by policy")
		return
	}

	author := pullReq.GetUser().GetLogin()
	kind := pr.KindOf(author)
	if kind == "" {
		run.set(pr.StatusUntrustedAuthor, fmt.Sprintf("author %q is not a trusted bot", author))
		return
	}
	updateType := pr.ClassifyUpdate(kind, pullReq.GetTitle(), pullReq.GetBody())
	status.SetClassification(idx, kind, updateType)

	if pullReq.GetMerged() {
		run.set(pr.StatusAlreadyMerged, "")
		return
	}
	if pullReq.GetHead().GetRepo().GetFork() {
		run.set(pr.StatusSkipped, "head branch lives in a fork")
		return
	}
	if pullReq.GetMergeableState() == "dirty" {
		if reason, detail := p.classifyObsolete(ctx, run.info, pullReq, nil); reason != "" {
			run.markObsolete(reason, detail)
			return
		}
		run.set(pr.StatusConflict, "merge conflict")
		return
	}
	if pullReq.GetAutoMerge() != nil && !p.MergeAutoMerge {
		run.set(pr.StatusAutoMerge, "auto-merge enabled; GitHub merges it")
		return
	}

	run.set(pr.StatusChecking, "")
	if !p.evaluateChecks(ctx, run) {
		return
	}

	if !resolved.Eligible(kind, updateType) {
		run.set(pr.StatusHeld, heldDetail(kind, updateType))
		return
	}

	if p.DryRun {
		detail := "dry-run: would " + p.plannedWrites()
		if p.Actions.Has(ActionApprove) {
			if err := p.ensureWriteAccess(ctx, run.info.Owner, run.info.Repo); err != nil {
				detail = withNote(detail, writeAccessDetail(err))
			}
		}
		run.set(pr.StatusSkipped, detail)
		return
	}

	if p.Actions.Has(ActionApprove) {
		if err := p.approve(ctx, run); err != nil {
			return
		}
	}
	if !p.Actions.Has(ActionMerge) {
		run.set(pr.StatusEligible, withNote("eligible; merge not in actions", preexistingNote(run)))
		return
	}
	p.merge(ctx, run)
}

// plannedWrites names the writes a dry run would perform on a green,
// eligible PR.
func (p *Processor) plannedWrites() string {
	var steps []string
	if p.Actions.Has(ActionApprove) {
		steps = append(steps, "approve")
	}
	if p.Actions.Has(ActionMerge) {
		steps = append(steps, "merge (squash)")
	}
	if len(steps) == 0 {
		return "label only"
	}
	return strings.Join(steps, ", ")
}

// finish runs on every exit: it attaches a prior rescue marker to failure
// outcomes, writes the classification label and appends the notes. A PR of
// a repository the policy excluded gets none of it.
func (p *Processor) finish(ctx context.Context, run *prRun) {
	if run.untouched {
		return
	}
	p.attachRescueMarker(ctx, run)
	state := run.status.StateAt(run.idx)
	if class := pr.LabelClass(state); class != "" && !p.DryRun {
		p.setLabel(ctx, run, class)
	}
	if len(run.notes) > 0 {
		entry := run.status.Snapshot()[run.idx]
		run.set(entry.State, withNote(entry.Detail, strings.Join(run.notes, "; ")))
	}
}

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

		if len(required.Failed) > 0 || outcome.state == stateFailure || outcome.state == stateError {
			if !p.classifyFailure(ctx, run, outcome, prot) {
				return false
			}
		}

		if required.allGreen() && outcome.state != statePending {
			return true
		}

		run.set(pr.StatusWaitingChecks, waitingDetail(required, outcome.state))
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
	record := func(name string, success, failed bool) {
		recordContext(reported, name, success, failed, time.Time{})
	}

	if checkRuns.GetTotal() == 0 && len(combined.Statuses) == 0 {
		return checkOutcome{state: stateSuccess, sha: combined.GetSHA(), reported: reported}, nil
	}

	var failedChecks []string
	var blockedChecks []string
	var noVerdictChecks []noVerdictCheck
	allComplete := true
	hasFailure := false
	for _, cr := range checkRuns.CheckRuns {
		name := cr.GetName()
		if cr.GetStatus() != statusCompleted {
			allComplete = false
			record(name, false, false)
			continue
		}
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
				record(name, false, false)
				continue
			case kindNoVerdict:
				if name != "" {
					noVerdictChecks = append(noVerdictChecks, noVerdictCheck{Name: name, Reason: reason})
				}
				record(name, false, false)
				continue
			}
			hasFailure = true
			if name != "" {
				failedChecks = append(failedChecks, name)
			}
			record(name, false, true)
			continue
		}
		// Neutral and skipped conclusions count as a pass for the guard
		// the way GitHub counts them for required checks.
		record(name, conclusion == stateSuccess || conclusion == "neutral" || conclusion == "skipped", false)
	}

	var statusTargets map[string]string
	hasStatusFailure := false
	for _, s := range combined.Statuses {
		state := s.GetState()
		name := s.GetContext()
		if state != stateFailure && state != stateError {
			record(name, state == stateSuccess, false)
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
			record(name, false, false)
			continue
		}
		record(name, false, true)
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
		noVerdictChecks: noVerdictChecks,
		reported:        reported,
		statusTargets:   statusTargets,
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

// attachRescueMarker attaches the newest rescue-attempt marker to failure
// and stale outcomes, so the operator can tell "needs a first rescue" apart
// from "a rescue already failed here". Staleness is decided against the
// PR's current head and content (see markStale).
func (p *Processor) attachRescueMarker(ctx context.Context, run *prRun) {
	switch run.status.StateAt(run.idx) {
	case pr.StatusFailed, pr.StatusFailedSecurity, pr.StatusConflict, pr.StatusStale, pr.StatusRefreshed, pr.StatusHeld:
	default:
		return
	}
	if run.status.RescueAt(run.idx) != nil {
		return
	}
	marker := run.rescueMarker(ctx, p)
	if marker == nil {
		return
	}
	p.markStale(ctx, run.info, run.pull, marker)
	run.status.SetRescue(run.idx, marker)
}

// noOpDetail explains a no-op classification. The shape is always the same
// one, so the text is a constant.
const noOpDetail = "no semantic change: the pinned action SHA is unchanged, only its version comment moved"

// compareFileLimit is the number of files the compare API returns at most.
// A comparison that hits it is truncated without saying so, and a no-op
// verdict read off a partial file list would be a guess. Staleness reads
// only the commit counts, so the limit does not concern it.
const compareFileLimit = 300

// classifyObsolete reports whether a PR is not worth fixing, and why: a
// sibling PR carries a higher version of the same dependency, or the diff
// changes nothing that executes. An empty reason means neither holds.
//
// Supersession is read from the sweep's PR list, so it costs no request and
// is decided first. cmp is the base...head comparison when the caller
// already holds it, and nil when it has to be fetched; a comparison that
// cannot be had only rules out the no-op verdict.
func (p *Processor) classifyObsolete(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest, cmp *github.CommitsComparison) (pr.ObsoleteReason, string) {
	if detail, ok := p.SupersededBy[pr.PRKey(info)]; ok {
		return pr.ReasonSuperseded, detail
	}
	if cmp == nil {
		cmp = p.compare(ctx, info, pullReq)
	}
	if cmp != nil && len(cmp.Files) < compareFileLimit && pr.NoOpDiff(cmp.Files) {
		return pr.ReasonNoOp, noOpDetail
	}
	return "", ""
}

// compare fetches the base...head comparison of a PR, or nil when it cannot
// be had. Callers treat nil as "no evidence": an unknown diff never softens
// a failure.
func (p *Processor) compare(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest) *github.CommitsComparison {
	base, head := pullReq.GetBase().GetRef(), pullReq.GetHead().GetSHA()
	if base == "" || head == "" {
		return nil
	}
	// per_page bounds the commit list; the file list has its own hard limit.
	cmp, _, err := p.Client.Repositories.CompareCommits(ctx, info.Owner, info.Repo, base, head, &github.ListOptions{PerPage: 1})
	if err != nil {
		return nil
	}
	return cmp
}

// securityPatterns returns the normalized security-check pattern list to
// use for classification: nil SecurityCheckPatterns falls back to the
// built-in defaults; a non-nil empty slice disables classification.
// securityPatterns returns the normalized security-check pattern list: the
// built-in defaults plus whatever the caller added.
func (p *Processor) securityPatterns() []string {
	patterns := append([]string{}, defaultSecurityCheckPatterns...)
	patterns = append(patterns, p.SecurityCheckPatterns...)
	return normalizePatterns(patterns)
}

// failureDetail builds a human-readable detail string for a non-security
// check failure, naming the failing checks when available.
func failureDetail(failedChecks []string) string {
	if len(failedChecks) == 0 {
		return "checks failed"
	}
	return fmt.Sprintf("checks failed: %s", joinCapped(failedChecks))
}

// joinCapped joins parts with ", ", naming at most detailMaxChecks of them
// and counting the rest, so a detail stays readable on one line.
func joinCapped(parts []string) string {
	const detailMaxChecks = 3
	if len(parts) <= detailMaxChecks {
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(parts[:detailMaxChecks], ", "), len(parts)-detailMaxChecks)
}

// withNote appends an operator note (for instance why a CircleCI build could
// not be inspected) to a status detail. An empty note leaves the detail as
// is.
func withNote(detail, note string) string {
	if note == "" {
		return detail
	}
	if detail == "" {
		return note
	}
	return detail + "; " + note
}

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
				run.set(pr.StatusConflict, "merge conflict")
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
			run.set(pr.StatusConflict, "merge conflict on retry")
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

// updateBranch brings the PR up to date with its base (the "Update branch"
// button). GitHub schedules the update and answers 202, which go-github
// surfaces as an AcceptedError: that is the success path.
func (p *Processor) updateBranch(ctx context.Context, run *prRun, why string) {
	_, _, err := p.Client.PullRequests.UpdateBranch(ctx, run.info.Owner, run.info.Repo, run.info.Number, nil)
	var accepted *github.AcceptedError
	if err != nil && !errors.As(err, &accepted) {
		run.set(pr.StatusFailed, ghErrorDetail("update-branch failed", err))
		return
	}
	run.set(pr.StatusRefreshed, "re-checking; "+why)
	p.postOnce(ctx, run, pr.MarkerKindEvidence, "update-branch", why)
}

func ghErrorDetail(prefix string, err error) string {
	msg := ""
	if ghErr, ok := errors.AsType[*github.ErrorResponse](err); ok {
		for _, e := range ghErr.Errors {
			if e.Message != "" {
				msg = e.Message
				break
			}
		}
		if msg == "" && ghErr.Message != "" {
			msg = ghErr.Message
		}
	}
	if msg == "" {
		msg = err.Error()
	}
	if prefix == "" {
		return msg
	}
	return fmt.Sprintf("%s: %s", prefix, msg)
}
