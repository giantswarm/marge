package process

import (
	"context"
	"fmt"

	"github.com/giantswarm/marge/internal/pr"
)

// changelogEntry writes the team's changelog entry on a PR the sweep would
// merge, and reports whether the sweep must stop on this PR for this run.
//
// It runs before the approval, never after: the entry is a commit, and a
// commit pushed after an approval dismisses it wherever the branch protection
// dismisses stale reviews, so an entry written later would break the merge it
// documents.
//
// The commit starts CI again, so the checks this sweep read no longer describe
// the head. The PR therefore waits: this run stops here and the next sweep
// classifies the new head and merges it. Merging on the checks of the commit
// before the entry would merge code no CI ran on.
//
// A PR that already carries the line is left alone and the sweep carries on
// with it, so a team does not stall on the same PR every day. A repository
// that keeps no changelog costs one read and nothing else: that is the common
// case, not a failure, and it is not reported as one.
func (p *Processor) changelogEntry(ctx context.Context, run *prRun, resolved pr.Policy) bool {
	if !p.Actions.Has(ActionChangelog) || p.DryRun {
		return false
	}
	if !resolved.Changelog.Enabled {
		return false
	}
	// The entry describes an update that will land. A PR the policy holds
	// for a person may never land, and one whose size marge could not read
	// has nothing to say.
	if !resolved.Eligible(run.kind, run.updateType) {
		return false
	}
	// An entry names what moved and where it landed, so a title that names
	// no version earns none: an Align files PR, a Herald PR and a bot's own
	// onboarding PR each move nothing a changelog records.
	// The fetched PR is the authority on the title, as it is for the kind
	// and the update type: a discovery entry may carry none.
	title := run.pull.GetTitle()
	dependency := pr.ExtractDependencyName(title)
	from, to := pr.ExtractVersions(title)
	if dependency == "" || to == "" {
		return false
	}

	outcome, err := pr.WriteChangelogEntry(ctx, p.Client, resolved.Changelog, pr.ChangelogFacts{
		Dependency: dependency,
		From:       from,
		To:         to,
		PR:         fmt.Sprintf("%s/%s#%d", run.info.Owner, run.info.Repo, run.info.Number),
		Repository: run.info.Owner + "/" + run.info.Repo,
		Kind:       string(run.kind),
		UpdateType: string(run.updateType),
	}, run.info.Owner, run.info.Repo, run.pull.GetHead().GetRef(), false)
	switch {
	case err != nil:
		// A changelog is a courtesy, never a guard: a repository without the
		// file, or a write GitHub refused, leaves the sweep to decide the PR
		// as it would have.
		run.note("changelog entry not written: " + err.Error())
		return false
	case !outcome.Written:
		return false
	}

	run.set(pr.StatusWaitingChecks, "changelog entry added; CI is running again on the new head")
	return true
}
