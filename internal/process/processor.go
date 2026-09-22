package process

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/logs"
	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
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

	// RebaseWait bounds the second pass over the PRs a merge of this sweep
	// made dirty (see Revisit). Zero uses the default (3m).
	RebaseWait time.Duration

	// Actions selects the steps this sweep performs. Nil performs all.
	Actions ActionSet

	// AppWriteAccess answers the write-access guard when the sweep
	// authenticates as the GitHub App. Nil is a person's token.
	AppWriteAccess AppWriteAccess

	// Rules is the catalogue loaded at the start of the sweep. Nil refuses
	// every remedy and leaves classification, approval and merging as they
	// are.
	Rules *rules.Catalogue
	// Remedies is the action vocabulary a rule may name. Nil refuses every
	// rule-selected remedy; the sweep's own update-branch still runs, out of
	// the built-in vocabulary.
	Remedies *remedy.Registry
	// Logs reads the excerpt a rule's log signal matches against. Nil leaves
	// every log signal unmatched.
	Logs *logs.Fetcher

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
	rebaseQueue
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

	// kind and updateType are the classification of the PR's author and its
	// dependency change.
	kind       pr.Kind
	updateType pr.UpdateType
	// failing names the red checks on the head that produced a verdict.
	failing []string
	// required is what the head reported for the base branch's required
	// contexts.
	required remedy.Required
	// reported counts the contexts the head reported in any state, and
	// settledAt is the newest completion among them. checksPending reports
	// whether one of them has not finished. Together they say whether a
	// context nobody reported may still report.
	reported      int
	checksPending bool
	settledAt     time.Time
	// files are the paths of the PR diff, fetched once and only for a rule
	// that carries a file signal.
	files       []string
	filesLoaded bool
	// statusTargets and detailsURLs say where each failing check's log
	// lives: a CircleCI build behind a commit status, an Actions job behind
	// a check run.
	statusTargets map[string]string
	detailsURLs   map[string]string
	// excerpts memoises one log excerpt per check and source.
	excerpts map[string]string
}

// checkURL says where a failing check's build or job lives: a CircleCI
// build behind a commit status, an Actions job behind a check run.
func (r *prRun) checkURL(check string) string {
	if url := r.statusTargets[check]; url != "" {
		return url
	}
	return r.detailsURLs[check]
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
	// A stopped run leaves the PRs it never decided as they are. A PR the
	// sweep did not look at is not a failed PR, and the summary of a
	// stopped run counts it as one it never reached.
	if ctx.Err() != nil {
		return
	}
	pullReq, _, err := p.Client.PullRequests.Get(ctx, info.Owner, info.Repo, info.Number)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		status.Update(idx, pr.StatusFailed, ghErrorDetail("fetch error", err))
		return
	}
	run := &prRun{info: info, pull: pullReq, status: status, idx: idx}
	defer p.finish(ctx, run)

	resolved := p.Policies.For(info.Owner, info.Repo)
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
	run.kind, run.updateType = kind, updateType
	status.SetClassification(idx, kind, updateType)

	if pullReq.GetMerged() {
		run.set(pr.StatusAlreadyMerged, "")
		return
	}
	if headRepo := pullReq.GetHead().GetRepo(); headRepo == nil ||
		!strings.EqualFold(headRepo.GetFullName(), info.Owner+"/"+info.Repo) {
		run.set(pr.StatusSkipped, "head branch lives in another repository")
		return
	}
	if pullReq.GetMergeableState() == "dirty" {
		if reason, detail := p.classifyObsolete(ctx, run.info, pullReq, nil); reason != "" {
			run.markObsolete(reason, detail)
			return
		}
		p.setConflict(run, "merge conflict")
		return
	}
	run.set(pr.StatusChecking, "")
	if !p.evaluateChecks(ctx, run) {
		return
	}

	if !resolved.Eligible(kind, updateType) {
		run.set(pr.StatusHeld, heldDetail(kind, updateType))
		p.postOnce(ctx, run, pr.MarkerKindEvidence, pr.MarkerOutcomeHeld, heldMarkerReason(kind, updateType, resolved))
		return
	}

	p.recordMergeFinding(ctx, run)

	if p.DryRun {
		detail := "dry-run: would " + p.plannedWrites(run, p.changelogApplies(ctx, run, resolved))
		if p.Actions.Has(ActionApprove) {
			if err := p.ensureWriteAccess(ctx, run.info.Owner, run.info.Repo); err != nil {
				run.set(pr.StatusSkipped, withNote(detail, writeAccessDetail(err)))
				return
			}
		}
		run.set(pr.StatusEligible, detail)
		return
	}

	// The entry goes on a head the checks have already passed, so it costs
	// the PR one more CI cycle and no more. Writing it before the checks
	// were read would cost the same cycle and leave the line on the branch
	// for the whole of it, where a rebase of the bot's own takes it away.
	if p.changelogEntry(ctx, run, resolved) {
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
	if p.handsOffToAutoMerge(run) {
		p.handOffToAutoMerge(ctx, run)
		return
	}
	p.merge(ctx, run)
}

// handsOffToAutoMerge reports whether GitHub, not marge, performs the merge.
func (p *Processor) handsOffToAutoMerge(run *prRun) bool {
	return run.pull.GetAutoMerge() != nil && !p.MergeAutoMerge
}

// handOffToAutoMerge leaves the merge to GitHub. GitHub fires auto-merge only
// once every requirement is met and does nothing to meet one, so a branch
// behind its base is brought up to date first; without that the PR waits
// indefinitely for a person who does not know they are needed.
func (p *Processor) handOffToAutoMerge(ctx context.Context, run *prRun) {
	if run.pull.GetMergeableState() == "behind" {
		p.updateBranch(ctx, run, "behind base; auto-merge needs an up-to-date branch")
		return
	}
	run.set(pr.StatusAutoMerge, "auto-merge enabled; GitHub merges it")
}

// plannedWrites names the writes a dry run would perform on a green,
// eligible PR. changelog says whether the PR earns a changelog entry, which
// only a read of the repository answers.
func (p *Processor) plannedWrites(run *prRun, changelog bool) string {
	var steps []string
	if changelog {
		steps = append(steps, "write the changelog entry")
	}
	if p.Actions.Has(ActionApprove) {
		steps = append(steps, "approve")
	}
	switch {
	case !p.Actions.Has(ActionMerge):
	case p.handsOffToAutoMerge(run):
		steps = append(steps, "leave the merge to auto-merge")
	default:
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
	p.applyRule(ctx, run)
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

// updateBranch brings the PR up to date with its base (the "Update branch"
// button) through the remedy action, so the hand-written path and a rule
// that names update-branch run the same code behind the same guards.
func (p *Processor) updateBranch(ctx context.Context, run *prRun, why string) {
	outcome, err := p.remedies().Apply(ctx, remedy.UpdateBranch, p.actionRequest(ctx, run), nil)
	switch {
	case err != nil:
		run.set(pr.StatusFailed, ghErrorDetail("update-branch failed", err))
	case outcome.Refused != "":
		run.note("update-branch refused: " + outcome.Refused)
	default:
		run.set(pr.StatusRefreshed, "re-checking; "+why)
		p.postOnce(ctx, run, pr.MarkerKindEvidence, string(remedy.UpdateBranch), why)
	}
}

// remedies is the action vocabulary, defaulting to the built-in one so a
// Processor built without it still performs its own actions. It only reads
// the field: the PRs of a sweep run together on one Processor, and a lazy
// assignment here would be a write to shared state from every one of them.
func (p *Processor) remedies() *remedy.Registry {
	if p.Remedies != nil {
		return p.Remedies
	}
	return remedy.Default()
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
