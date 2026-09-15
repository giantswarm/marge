package process

import (
	"context"
	"fmt"
	"time"

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
		key := string(source) + " " + check
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

func (p *Processor) request(ctx context.Context, run *prRun, hit *rules.Hit) *remedy.Request {
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
		Check:             hit.Check,
		CheckURL:          run.checkURL(hit.Check),
		LogMatched:        hit.LogMatched,
		MissingContexts:   hit.MissingContexts,
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
