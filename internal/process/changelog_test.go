package process

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

const changelogFile = `# Changelog

## [Unreleased]

### Changed

- Something else.
`

func changelogFixture() *guardFixture {
	f := greenFixture()
	f.title = "chore(deps): bump lodash from 4.17.20 to 4.17.21"
	f.changelog = changelogFile
	return f
}

// TestChangelog_writtenBeforeTheApproval guards the ordering the whole step
// exists for: the entry is a commit, so it lands before anything is approved,
// and the PR then waits for the CI that commit started rather than merging on
// the checks of a head that no longer exists.
func TestChangelog_writtenBeforeTheApproval(t *testing.T) {
	f := changelogFixture()

	got := f.run(t, func(p *Processor) { p.Policies = policySet(t, "changelog:\n  enabled: true\n", nil) })

	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.Contains(t, got.Detail, "changelog entry added")
	require.Equal(t, int32(1), f.changelogPuts.Load())
	require.Contains(t, f.changelog, "- Update lodash from 4.17.20 to 4.17.21")
	// Nothing is approved and nothing is merged in the run that wrote it.
	require.Zero(t, f.approveCalls.Load())
	require.Zero(t, f.mergeCalls.Load())
}

// TestChangelog_writtenOnce guards the promise a second sweep depends on: the
// entry is in the file, so the next run writes nothing and decides the PR as
// it would have.
func TestChangelog_writtenOnce(t *testing.T) {
	f := changelogFixture()
	f.changelog = strings.Replace(
		changelogFile,
		"- Something else.",
		"- Something else.\n- Update lodash from 4.17.20 to 4.17.21 (org/repo#7)",
		1,
	)

	got := f.run(t, func(p *Processor) { p.Policies = policySet(t, "changelog:\n  enabled: true\n", nil) })

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
}

// TestChangelog_writtenWithoutAnyPolicyFile: the entry is the company
// default, so a team that writes no policy file still records its updates.
func TestChangelog_writtenWithoutAnyPolicyFile(t *testing.T) {
	f := changelogFixture()

	got := f.run(t, nil)

	require.Equal(t, int32(1), f.changelogPuts.Load())
	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
}

// TestChangelog_offWhenTheTeamSwitchesItOff: a team that does not want its
// bot PRs to carry an entry says so, and the sweep merges as it always did.
func TestChangelog_offWhenTheTeamSwitchesItOff(t *testing.T) {
	f := changelogFixture()

	got := f.run(t, func(p *Processor) {
		p.Policies = policySet(t, "changelog:\n  enabled: false\n", nil)
	})

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
}

// TestChangelog_notInAClassifyRun: the portal's Classify now runs the classify
// step alone, and a classification never commits to a branch.
func TestChangelog_notInAClassifyRun(t *testing.T) {
	f := changelogFixture()

	got := f.run(t, func(p *Processor) {
		p.Policies = policySet(t, "changelog:\n  enabled: true\n", nil)
		p.Actions = ActionSet{ActionClassify: true}
	})

	require.Zero(t, f.changelogPuts.Load())
	require.NotEqual(t, pr.StatusWaitingChecks, got.State, got.Detail)
}

// TestChangelog_aRepositoryWithoutTheFileIsDecidedAnyway: plenty of
// repositories keep no changelog. That is a normal state, not a failure, so
// the sweep says nothing about it and decides the PR as it always did.
func TestChangelog_aRepositoryWithoutTheFileIsDecidedAnyway(t *testing.T) {
	f := changelogFixture()
	f.changelog = ""

	got := f.run(t, nil)

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.NotContains(t, got.Detail, "changelog")
}

// TestChangelog_notWhenTheReleaseNotesAreGenerated is the scope of the whole
// step: a repository with a cliff.toml publishes the bump on its release page
// already, and it no longer cuts a version section, so a line in the file
// would repeat the release under a heading nothing releases.
func TestChangelog_notWhenTheReleaseNotesAreGenerated(t *testing.T) {
	f := changelogFixture()
	f.generatedReleaseNotes = true

	got := f.run(t, nil)

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.NotContains(t, got.Detail, "changelog")
}

// TestChangelog_writtenAfterTheChecks guards the placement: a PR whose checks
// are not green earns no entry, so the line never waits out a CI cycle on a
// branch the bot can rebase under it.
func TestChangelog_writtenAfterTheChecks(t *testing.T) {
	f := changelogFixture()
	f.headChecks["go-build"] = "pending"

	got := f.run(t, nil)

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.Contains(t, got.Detail, "pending: go-build")
}

// TestChangelog_notOnAPRTheTeamHolds: an entry describes an update that will
// land. A major update a person has to decide on may never land.
func TestChangelog_notOnAPRTheTeamHolds(t *testing.T) {
	f := changelogFixture()
	f.title = "chore(deps): update module github.com/foo/bar to v5.0.0"

	got := f.run(t, func(p *Processor) {
		p.Policies = policySet(t, "updateTypes:\n  renovate:\n    - patch\n", nil)
	})

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusHeld, got.State, got.Detail)
}

// TestChangelog_dryRunWritesNothingAndNamesTheEntry: a dry run reads the
// repository so it can say what a real run would write, and writes nothing.
func TestChangelog_dryRunWritesNothingAndNamesTheEntry(t *testing.T) {
	f := changelogFixture()

	got := f.run(t, func(p *Processor) { p.DryRun = true })

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusEligible, got.State, got.Detail)
	require.Contains(t, got.Detail, "write the changelog entry")
}

// TestChangelog_dryRunSaysNothingWhereTheNotesAreGenerated: the same run over
// a repository that earns no entry must not promise one.
func TestChangelog_dryRunSaysNothingWhereTheNotesAreGenerated(t *testing.T) {
	f := changelogFixture()
	f.generatedReleaseNotes = true

	got := f.run(t, func(p *Processor) { p.DryRun = true })

	require.Equal(t, pr.StatusEligible, got.State, got.Detail)
	require.NotContains(t, got.Detail, "changelog")
}
