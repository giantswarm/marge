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

// TestChangelog_offWithoutTheTeamAskingForIt: the entry is a commit on a
// team's branch, so no team gets one until its policy says so.
func TestChangelog_offWithoutTheTeamAskingForIt(t *testing.T) {
	f := changelogFixture()

	got := f.run(t, nil)

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

// TestChangelog_aRepositoryWithoutTheFileIsDecidedAnyway: a changelog is a
// courtesy, never a guard.
func TestChangelog_aRepositoryWithoutTheFileIsDecidedAnyway(t *testing.T) {
	f := changelogFixture()
	f.changelog = ""

	got := f.run(t, func(p *Processor) { p.Policies = policySet(t, "changelog:\n  enabled: true\n", nil) })

	require.Zero(t, f.changelogPuts.Load())
	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.Contains(t, got.Detail, "changelog entry not written")
}
