package process

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
)

// policySet resolves a policy set from one team file and the exceptions of
// the repositories that deviate from it.
func policySet(t *testing.T, teamFile string, exceptions map[string]policy.Exception) *policy.Set {
	t.Helper()
	doc, err := policy.ParseDocument(policy.TeamFile("bumblebee"), teamFile)
	require.NoError(t, err)
	set, err := policy.NewSet([]policy.File{{Path: policy.TeamFile("bumblebee"), Doc: doc}}, exceptions)
	require.NoError(t, err)
	return set
}

// TestPolicy_recordedInTheOutcome holds the promise that every sweep
// decision can be explained afterwards: the policy the PR was decided
// under is on its outcome entry, with the files that produced it.
func TestPolicy_recordedInTheOutcome(t *testing.T) {
	fixture := greenFixture()
	entry := fixture.run(t, func(p *Processor) {
		p.Policies = policySet(t, "slackChannel: team-bumblebee\n", nil)
	})

	require.Equal(t, pr.StatusMerged, entry.State)
	require.NotNil(t, entry.Policy)
	require.True(t, entry.Policy.Sweep)
	require.Equal(t, "team-bumblebee", entry.Policy.SlackChannel)
	require.Equal(t, []string{"built-in company defaults", policy.TeamFile("bumblebee")}, entry.Policy.Sources)
	require.Equal(t, []pr.UpdateType{pr.UpdatePatch, pr.UpdateMinor, pr.UpdateDigest, pr.UpdatePin, pr.UpdateLockfile}, entry.Policy.UpdateTypes[pr.KindRenovate])
}

// TestPolicy_heldByTeamDeviation merges nothing a team excluded, and says
// which policy held the PR.
func TestPolicy_heldByTeamDeviation(t *testing.T) {
	fixture := greenFixture()
	// The PR is a grouped non-major update, so it is a minor.
	entry := fixture.run(t, func(p *Processor) {
		p.Policies = policySet(t, "updateTypes:\n  renovate: [patch]\n", nil)
	})

	require.Equal(t, pr.StatusHeld, entry.State)
	require.Contains(t, entry.Detail, "minor update waits for a person")
	require.Equal(t, pr.UpdateMinor, entry.UpdateType)
	require.Equal(t, 0, int(fixture.mergeCalls.Load()))
	require.Equal(t, 0, int(fixture.approveCalls.Load()))
	require.NotNil(t, entry.Policy)
	require.Equal(t, []pr.UpdateType{pr.UpdatePatch}, entry.Policy.UpdateTypes[pr.KindRenovate])
}

// TestPolicy_exceptionSwitchesTheSweepOff skips every PR of a repository
// whose exception says so, and writes nothing to it.
func TestPolicy_exceptionSwitchesTheSweepOff(t *testing.T) {
	off := false
	fixture := greenFixture()
	entry := fixture.run(t, func(p *Processor) {
		p.Policies = policySet(t, "", map[string]policy.Exception{"repo": {Enabled: &off}})
	})

	require.Equal(t, pr.StatusSkipped, entry.State)
	require.Contains(t, entry.Detail, "sweep switched off for this repository by policy")
	require.Equal(t, 0, int(fixture.mergeCalls.Load()))
	require.Equal(t, 0, int(fixture.approveCalls.Load()))
	require.Equal(t, 0, int(fixture.updateBranchCalls.Load()))
	require.Equal(t, 0, int(fixture.labelAdds.Load()), "an excluded repository receives no write at all")
	require.Equal(t, 0, int(fixture.labelRemoves.Load()))
	require.Equal(t, 0, int(fixture.commentPosts.Load()))
	require.Empty(t, entry.Label)
	require.NotNil(t, entry.Policy)
	require.False(t, entry.Policy.Sweep)
	require.Contains(t, entry.Policy.Sources, "botPRsSweep of repository repo")
}

// TestPolicy_nilSetAppliesCompanyDefaults keeps a processor without a
// policy set working: the company defaults apply, so a zero Processor
// behaves like a sweep whose policy files were all absent.
func TestPolicy_nilSetAppliesCompanyDefaults(t *testing.T) {
	entry := greenFixture().run(t, nil)

	require.Equal(t, pr.StatusMerged, entry.State)
	require.NotNil(t, entry.Policy)
	require.Equal(t, pr.CompanyDefaults(), *entry.Policy)
}
