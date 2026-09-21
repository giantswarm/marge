package process

import (
	"context"
	"fmt"

	"github.com/giantswarm/marge/internal/pr"
)

// changelogApplies reports whether this PR earns a changelog entry: the step
// is on, the team wants entries, the title names an update, and the repository
// cuts its release from the changelog file.
//
// A repository that generates its release notes from its commits publishes the
// update already -- git-cliff groups a bot's commit like any other -- and it
// cuts no version section, so an entry there repeats the release page under a
// heading nothing releases. That is the common case, not a failure, and it is
// not reported as one.
//
// An entry names what moved and where it landed, so a title that names no
// version earns none: an Align files PR, a Herald PR and a bot's own
// onboarding PR each move nothing a changelog records. The fetched PR is the
// authority on the title, as it is for the kind and the update type: a
// discovery entry may carry none.
func (p *Processor) changelogApplies(ctx context.Context, run *prRun, resolved pr.Policy) bool {
	if !p.Actions.Has(ActionChangelog) || !resolved.Changelog.Enabled {
		return false
	}
	if dependency, to := changelogUpdate(run); dependency == "" || to == "" {
		return false
	}
	generated, err := pr.ReleaseNotesGenerated(ctx, p.Client, run.info.Owner, run.info.Repo, "")
	if err != nil {
		// A changelog is a courtesy, never a guard: a read GitHub refused
		// leaves the sweep to decide the PR as it would have, and says so.
		run.note("changelog entry not written: " + err.Error())
		return false
	}
	return !generated
}

// changelogEntry writes the team's changelog entry on a PR the sweep is about
// to approve, and reports whether the sweep must stop on this PR for this run.
//
// It runs after the checks and before the approval. After the checks, because
// the entry is a commit and the bot rebases its own branch: a line written
// before the checks were read waits out the whole CI cycle on a branch the bot
// may force-push, and is lost. Before the approval, because a commit pushed
// after an approval dismisses it wherever the branch protection dismisses
// stale reviews.
//
// The commit starts CI again, so the checks this sweep read no longer describe
// the head. The PR therefore waits: this run stops here and the next sweep
// classifies the new head and merges it. Merging on the checks of the commit
// before the entry would merge code no CI ran on.
//
// A PR that already carries the line is left alone and the sweep carries on
// with it, so a team does not stall on the same PR every day.
func (p *Processor) changelogEntry(ctx context.Context, run *prRun, resolved pr.Policy) bool {
	if p.DryRun || !p.changelogApplies(ctx, run, resolved) {
		return false
	}
	// The entry is a commit, so it needs the same access the approval does.
	// The approval reports its own refusal; this one only stands aside.
	if err := p.ensureWriteAccess(ctx, run.info.Owner, run.info.Repo); err != nil {
		run.note("changelog entry not written: " + writeAccessDetail(err))
		return false
	}

	dependency, to := changelogUpdate(run)
	from, _ := pr.ExtractVersions(run.pull.GetTitle())
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
		run.note("changelog entry not written: " + err.Error())
		return false
	case !outcome.Written:
		// The repository keeps no changelog, or the entry is already in it.
		// Neither is a failure and neither is worth a word on the outcome.
		return false
	}

	run.set(pr.StatusWaitingChecks, "changelog entry added; CI is running again on the new head")
	return true
}

// changelogUpdate reads the PR title as an update: what moves, and the version
// it moves to.
func changelogUpdate(run *prRun) (dependency, to string) {
	title := run.pull.GetTitle()
	_, to = pr.ExtractVersions(title)
	return pr.ExtractDependencyName(title), to
}
