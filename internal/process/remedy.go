package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/marge/internal/logs"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
)

// applyRule runs the catalogue against a classified PR and applies the
// action the matching rule names. It is called from finish, the one point
// every classification passes, so no exit of ProcessPR can skip it.
//
// The rule contributes refusals; the action's own guards run first and are
// not addressable from the rule, so a rule can never do what its action
// forbids.
func (p *Processor) applyRule(ctx context.Context, run *prRun) {
	if !p.Actions.Has(ActionRemedy) || !p.Rules.Available() || p.Remedies == nil {
		return
	}
	state := run.status.StateAt(run.idx)
	if !rules.Remediable(state) {
		return
	}

	hit := p.Rules.Match(p.subject(ctx, run, state))
	if hit == nil {
		p.recordUnhandled(ctx, run)
		return
	}
	if p.DryRun {
		run.note(fmt.Sprintf("dry-run: rule %s would apply %s", hit.Rule.Name, hit.Rule.Action.Name))
		return
	}

	outcome, err := p.Remedies.Apply(ctx, hit.Rule.Action.Name, p.request(ctx, run, hit), hit.Rule.Guards())
	switch {
	case err != nil:
		run.note(fmt.Sprintf("rule %s failed: %s", hit.Rule.Name, err))
	case outcome.Refused != "":
		run.note(fmt.Sprintf("rule %s refused: %s", hit.Rule.Name, outcome.Refused))
	case outcome.Applied:
		if !outcome.KeepClassification {
			run.set(pr.StatusRemedied, fmt.Sprintf("%s: %s", hit.Rule.Name, outcome.Detail))
		} else {
			run.note(fmt.Sprintf("rule %s: %s", hit.Rule.Name, outcome.Detail))
		}
		p.postOnce(ctx, run, pr.MarkerKindEvidence, string(hit.Rule.Action.Name), evidenceReason(hit, p.Rules.Digest))
	}
	if outcome.StopRepository {
		run.note("sweep stopped for this repository")
	}
}

// evidenceReason names the rule and the catalogue it came from, so a PR
// says which version of which rule acted on it.
func evidenceReason(hit *rules.Hit, digest string) string {
	return fmt.Sprintf("%s (rule %s, catalogue %s)", hit.Rule.Evidence.Reason, hit.Rule.Name, digest)
}

// subject is what the catalogue matches against.
func (p *Processor) subject(ctx context.Context, run *prRun, state pr.StatusState) *rules.Subject {
	return &rules.Subject{
		State:           state,
		Kind:            run.kind,
		Title:           run.pull.GetTitle(),
		Failing:         run.failing,
		Pending:         run.pending,
		BaseState:       p.baseStates(ctx, run),
		MissingContexts: run.required.Missing,
		Files:           func() []string { return p.diffFiles(ctx, run) },
		Log:             p.logExcerpt(ctx, run),
	}
}

// logExcerpt returns the reader a rule's log signal uses. Every excerpt is
// memoised per check and source, so two rules reading the same log cost one
// fetch. A source with no reference for the check yields nothing, and the
// rule does not match.
func (p *Processor) logExcerpt(ctx context.Context, run *prRun) func(rules.LogSource, string, int) (string, bool) {
	if p.Logs == nil {
		return nil
	}
	return func(source rules.LogSource, check string, maxBytes int) (string, bool) {
		key := excerptKey(source, check)
		if cached, ok := run.excerpts[key]; ok {
			return cached, cached != ""
		}
		excerpt := ""
		switch source {
		case rules.LogActions:
			if url := run.detailsURLs[check]; url != "" {
				excerpt, _ = p.Logs.Actions(ctx, run.info.Owner, run.info.Repo, url, maxBytes)
			}
		case rules.LogCircleCI:
			if url := run.statusTargets[check]; url != "" {
				excerpt, _ = p.Logs.CircleCIBuild(ctx, url, maxBytes)
			}
		}
		if run.excerpts == nil {
			run.excerpts = make(map[string]string)
		}
		run.excerpts[key] = excerpt
		return excerpt, excerpt != ""
	}
}

// request is the action request of a rule match: the PR's own facts plus
// what made the rule match.
func (p *Processor) request(ctx context.Context, run *prRun, hit *rules.Hit) *remedy.Request {
	req := p.actionRequest(ctx, run)
	req.Check = hit.Check
	req.CheckURL = run.checkURL(hit.Check)
	req.LogMatched = hit.LogMatched
	req.MissingContexts = hit.MissingContexts
	req.Commands = hit.Commands
	return req
}

// actionRequest is what every action reads about a PR, whether a rule
// selected it or the sweep's own classification did.
func (p *Processor) actionRequest(ctx context.Context, run *prRun) *remedy.Request {
	return &remedy.Request{
		Info:              run.info,
		Pull:              run.pull,
		Kind:              run.kind,
		Update:            run.updateType,
		Head:              run.pull.GetHead().GetSHA(),
		DryRun:            p.DryRun,
		Failing:           run.failing,
		SecurityFailure:   classifySecurityFailure(run.failing, p.securityPatterns()),
		Required:          run.required,
		Base:              baseSplit(p.baseStates(ctx, run)),
		Now:               time.Now(),
		Reported:          run.reported,
		ChecksPending:     run.checksPending,
		ChecksSettledAt:   run.settledAt,
		AppliedThisChange: p.appliedThisChange(ctx, run),
		Deps: remedy.Deps{
			GitHub:   p.Client,
			CircleCI: p.CircleCI,
			Login:    p.Login,
		},
	}
}

// baseStates says what the base head reported for each failing check. A
// lookup that cannot be made leaves the map empty, and an absent entry is
// absent, never green.
func (p *Processor) baseStates(ctx context.Context, run *prRun) map[string]rules.CheckState {
	out := make(map[string]rules.CheckState, len(run.failing))
	if len(run.failing) == 0 {
		return out
	}
	baseSHA := run.pull.GetBase().GetSHA()
	if baseSHA == "" {
		return out
	}
	states, err := p.baseContextStates(ctx, run.info, baseSHA)
	if err != nil {
		return out
	}
	for _, name := range run.failing {
		st, ok := states[name]
		switch {
		case !ok:
			out[name] = rules.CheckAbsent
		case st.green():
			out[name] = rules.CheckGreen
		case st.failed:
			out[name] = rules.CheckRed
		default:
			out[name] = rules.CheckAbsent
		}
	}
	return out
}

// baseSplit partitions the failing checks the way an action reads them.
func baseSplit(states map[string]rules.CheckState) remedy.BaseStates {
	var split remedy.BaseStates
	for name, state := range states {
		switch state {
		case rules.CheckGreen:
			split.Green = append(split.Green, name)
		case rules.CheckRed:
			split.Red = append(split.Red, name)
		default:
			split.Absent = append(split.Absent, name)
		}
	}
	return split
}

// diffFiles returns the paths of the PR diff, or nothing when the
// comparison cannot be had. A rule with a file signal then does not match.
func (p *Processor) diffFiles(ctx context.Context, run *prRun) []string {
	if run.filesLoaded {
		return run.files
	}
	run.filesLoaded = true
	cmp := p.compare(ctx, run.info, run.pull)
	if cmp == nil || len(cmp.Files) >= compareFileLimit {
		return nil
	}
	run.files = make([]string, 0, len(cmp.Files))
	for _, f := range cmp.Files {
		run.files = append(run.files, f.GetFilename())
	}
	return run.files
}

// appliedThisChange names the actions an evidence marker records for the
// change currently on the branch, so an action runs once per change.
func (p *Processor) appliedThisChange(ctx context.Context, run *prRun) map[remedy.Name]bool {
	applied := make(map[remedy.Name]bool)
	head := run.pull.GetHead().GetSHA()
	for _, marker := range run.markers(ctx, p) {
		if !marker.IsEvidence() {
			continue
		}
		marker.MarkStale(head, func() pr.Fingerprint { return run.fingerprint(ctx, p) })
		if !marker.Stale {
			applied[remedy.Name(marker.Outcome)] = true
		}
	}
	return applied
}

// recordUnhandled notes a failure no rule recognised, with a signature that
// groups the same failure across PRs. It reads only excerpts the rules
// already fetched, so noticing a pattern costs no extra request.
//
// The signature is written on the PR as an evidence marker as well. The run
// ends and its grouping goes with it, so the marker is the only record that
// survives: `marge rules draft` builds a skeleton from it, and counting the
// PRs that carry one signature is what decides whether a pattern is worth a
// rule.
func (p *Processor) recordUnhandled(ctx context.Context, run *prRun) {
	if len(run.failing) == 0 {
		return
	}
	checks := slices.Clone(run.failing)
	slices.Sort(checks)

	excerpt := signatureExcerpt(checks, run.excerpts)
	unhandled := &pr.Unhandled{
		Signature: failureSignature(checks, excerpt),
		Checks:    checks,
		Excerpt:   excerpt,
	}
	if standing := p.standingUnhandled(ctx, run); standing != nil {
		unhandled.Signature = standing.Signature
		unhandled.Checks = standing.Checks
		unhandled.Excerpt = standing.Excerpt
	}
	run.status.SetUnhandled(run.idx, unhandled)

	p.postMarker(ctx, run, &pr.RescueMarker{
		Kind:      pr.MarkerKindEvidence,
		Outcome:   pr.MarkerOutcomeUnhandled,
		Reason:    strings.Join(unhandled.Checks, ", "),
		Signature: unhandled.Signature,
		Checks:    unhandled.Checks,
		Excerpt:   unhandled.Excerpt,
	})
}

// excerptKey names one excerpt of run.excerpts: one log of one check.
func excerptKey(source rules.LogSource, check string) string {
	return string(source) + " " + check
}

// signatureSources are the logs an excerpt can come from, in the order
// signatureExcerpt reads them.
var signatureSources = []rules.LogSource{rules.LogActions, rules.LogCircleCI}

// signatureExcerpt is the excerpt that identifies the failure. It reads
// only the excerpts the rules already fetched, for the checks the
// classification found failing, and it prefers one that names a failure. A
// build that was cancelled leaves the output of steps that succeeded, and a
// signature over such an excerpt groups PRs that failed differently.
func signatureExcerpt(checks []string, excerpts map[string]string) string {
	fallback := ""
	for _, check := range checks {
		for _, source := range signatureSources {
			raw := excerpts[excerptKey(source, check)]
			if raw == "" {
				continue
			}
			excerpt := signatureTail(logs.PlainText(raw))
			if logs.CarriesFailure(excerpt) {
				return excerpt
			}
			if fallback == "" {
				fallback = excerpt
			}
		}
	}
	return fallback
}

// standingUnhandled returns the unhandled marker already on the PR for the
// change on the branch, or nil. A later run reads back the signature the
// first one wrote, so one failure keeps one signature while the branch does
// not move -- a log that is truncated or unreachable this time does not
// split the group in two.
func (p *Processor) standingUnhandled(ctx context.Context, run *prRun) *pr.Unhandled {
	head := run.pull.GetHead().GetSHA()
	for _, marker := range run.markers(ctx, p) {
		if !marker.IsUnhandled() {
			continue
		}
		marker.MarkStale(head, func() pr.Fingerprint { return run.fingerprint(ctx, p) })
		if marker.Stale {
			continue
		}
		return &pr.Unhandled{Signature: marker.Signature, Checks: marker.Checks, Excerpt: marker.Excerpt}
	}
	return nil
}

// signatureBytes is how much of an excerpt identifies a failure. A rule's
// excerpt ends at the error the job reported, so its last lines are the
// failure; everything before it is the run that led there, and it carries
// runner versions and image digests that differ between two PRs failing the
// same way.
const signatureBytes = 2000

func signatureTail(excerpt string) string {
	if len(excerpt) <= signatureBytes {
		return excerpt
	}
	return excerpt[len(excerpt)-signatureBytes:]
}

// digitRE and hexRE blank out the parts of a log line that differ between
// two occurrences of one failure: build numbers, SHAs, timestamps.
var (
	digitRE = regexp.MustCompile(`\d+`)
	hexRE   = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
)

// failureSignature identifies the shape of a failure. Two PRs failing the
// same way share it, whatever their build numbers and commit SHAs.
func failureSignature(checks []string, excerpt string) string {
	normalised := hexRE.ReplaceAllString(strings.ToLower(excerpt), "#")
	normalised = digitRE.ReplaceAllString(normalised, "#")
	sum := sha256.Sum256([]byte(strings.Join(checks, "\n") + "\n" + normalised))
	return hex.EncodeToString(sum[:])[:12]
}
