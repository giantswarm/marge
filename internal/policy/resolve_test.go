package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// companyDefaultFile is the content of bot-prs-sweep/default.yaml as it is
// merged in giantswarm/github. The fixture tests resolve against it so a
// change to the real file is a change to these expectations.
const companyDefaultFile = `
updateTypes:
  renovate: [patch, minor, digest, pin, lockfile]
  dependabot: [patch, minor, digest, pin, lockfile]
  align-files: [none]
  herald: [none]
schedule: disabled
rescue:
  enabled: false
  timeout: 20m
  weekly: 5
  confirm: per-pr
concurrency:
  perTeam: 5
  perRepo: 1
modelConfig: default-model-config
`

// TestResolve_defaultTeamAndException resolves the whole chain per PR: the
// company defaults, one team's deviations and one repository's exception.
func TestResolve_defaultTeamAndException(t *testing.T) {
	teamFile := `
slackChannel: team-bumblebee
rescue:
  enabled: true
  timeout: 30m
  weekly: 3
  budget:
    perRescue: 3
    weekly: 15
  confirm: per-sweep
concurrency:
  perTeam: 8
updateTypes:
  renovate: [patch, minor]
`
	repositoriesFile := `
- name: marge
  componentType: cli
- name: muster
  componentType: service
  botPRsSweep:
    updateTypes: [patch]
    rescue: false
- name: klaus
  componentType: service
  botPRsSweep:
    enabled: false
`
	defaults, err := ParseDocument(DefaultFile, companyDefaultFile)
	require.NoError(t, err)
	team, err := ParseDocument(TeamFile("bumblebee"), teamFile)
	require.NoError(t, err)
	// The team file exists, so the schedule runs; see Loader.TeamScope.
	team = withSchedule(team, scheduleEnabled)

	repos, exceptions, err := ParseRepositories(repositoriesFile, "giantswarm", RepositoriesFile("bumblebee"))
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/marge", "giantswarm/muster", "giantswarm/klaus"}, repos)

	set, err := NewSet([]File{{Path: DefaultFile, Doc: defaults}, {Path: TeamFile("bumblebee"), Doc: team}}, exceptions)
	require.NoError(t, err)

	// A repository without an exception is swept under the team policy.
	plain := set.For("marge")
	require.True(t, plain.Sweep)
	require.True(t, plain.Schedule)
	require.Equal(t, "team-bumblebee", plain.SlackChannel)
	require.Equal(t, "default-model-config", plain.ModelConfig)
	require.Equal(t, 8, plain.Concurrency.PerTeam)
	require.Equal(t, 1, plain.Concurrency.PerRepo)
	require.Equal(t, pr.RescuePolicy{
		Enabled: true,
		Timeout: 30 * time.Minute,
		Weekly:  3,
		Budget:  pr.Budget{PerRescueUSD: 3, WeeklyUSD: 15},
		Confirm: pr.ConfirmPerSweep,
	}, plain.Rescue)
	require.True(t, plain.Eligible(pr.KindRenovate, pr.UpdateMinor))
	require.True(t, plain.Eligible(pr.KindAlignFiles, pr.UpdateNone))
	// The team narrowed Renovate to patch and minor, so a digest that the
	// company defaults merge now waits.
	require.False(t, plain.Eligible(pr.KindRenovate, pr.UpdateDigest))
	require.True(t, plain.Eligible(pr.KindDependabot, pr.UpdateDigest))
	require.False(t, plain.Eligible(pr.KindRenovate, pr.UpdateMajor))
	require.False(t, plain.Eligible(pr.KindRenovate, pr.UpdateUnknown))

	// The exception restricts the update types of every kind and switches
	// the rescues off; nothing else changes.
	restricted := set.For("muster")
	require.True(t, restricted.Sweep)
	require.False(t, restricted.Rescue.Enabled)
	require.Equal(t, 3, restricted.Rescue.Weekly)
	require.True(t, restricted.Eligible(pr.KindRenovate, pr.UpdatePatch))
	require.False(t, restricted.Eligible(pr.KindRenovate, pr.UpdateMinor))
	require.False(t, restricted.Eligible(pr.KindDependabot, pr.UpdateMinor))
	require.False(t, restricted.Eligible(pr.KindHerald, pr.UpdateNone))

	// The exception switches the sweep off for the repository alone.
	require.False(t, set.For("klaus").Sweep)
	require.True(t, set.For("marge").Sweep)

	// Matching is case-insensitive, the way GitHub matches repository names.
	require.False(t, set.For("KLAUS").Sweep)

	// Every resolved policy names the files it came from, exception last.
	require.Equal(t, []string{
		"built-in company defaults",
		DefaultFile,
		TeamFile("bumblebee"),
	}, plain.Sources)
	require.Equal(t, []string{
		"built-in company defaults",
		DefaultFile,
		TeamFile("bumblebee"),
		"botPRsSweep of repository muster",
	}, restricted.Sources)
	require.Equal(t, []string{DefaultFile, TeamFile("bumblebee")}, set.Files())
}

// TestResolve_noFiles resolves the company defaults on their own, the case
// of a team that has no policy file and of the query scope.
func TestResolve_noFiles(t *testing.T) {
	set, err := NewSet([]File{{Path: DefaultFile, Doc: nil}}, nil)
	require.NoError(t, err)
	resolved := set.For("marge")

	require.Equal(t, pr.CompanyDefaults(), resolved)
	require.False(t, resolved.Schedule, "a team without a policy file is never swept by the schedule")
	require.False(t, resolved.Rescue.Enabled)
	require.True(t, resolved.Eligible(pr.KindRenovate, pr.UpdatePatch))
	require.False(t, resolved.Eligible(pr.KindRenovate, pr.UpdateMajor))
	require.Empty(t, set.Files())
}

// TestResolve_scheduleKeyPauses proves the only switch a team needs: the
// file is the opt-in and the schedule key pauses it again.
func TestResolve_scheduleKeyPauses(t *testing.T) {
	paused, err := ParseDocument(TeamFile("shield"), "schedule: disabled\n")
	require.NoError(t, err)
	set, err := NewSet([]File{{Path: TeamFile("shield"), Doc: paused}}, nil)
	require.NoError(t, err)
	require.False(t, set.For("any").Schedule)
}

// TestResolve_exceptionOnlyNarrows refuses an exception that widens what
// the team allowed, instead of narrowing it silently.
func TestResolve_exceptionOnlyNarrows(t *testing.T) {
	off, err := ParseDocument(TeamFile("bumblebee"), "rescue:\n  enabled: false\n")
	require.NoError(t, err)

	yes := true
	_, err = NewSet([]File{{Path: TeamFile("bumblebee"), Doc: off}}, map[string]Exception{
		"marge": {Rescue: &yes},
	})
	require.ErrorContains(t, err, "cannot switch the rescues on")

	sweepOff, err := ParseDocument(DefaultFile, "")
	require.NoError(t, err)
	base, err := NewSet([]File{{Path: DefaultFile, Doc: sweepOff}}, map[string]Exception{
		"marge": {Enabled: &yes},
	})
	require.NoError(t, err, "switching a sweep on where it already is on is not a widening")
	require.True(t, base.For("marge").Sweep)
}

// TestPolicy_declaredUnenforced holds the promise that a cap nothing
// enforces is named rather than assumed.
func TestPolicy_declaredUnenforced(t *testing.T) {
	resolved := pr.CompanyDefaults()
	resolved.Rescue.Budget = pr.Budget{PerRescueUSD: 3, WeeklyUSD: 15}
	require.Empty(t, resolved.DeclaredUnenforced(), "an unenforced cap on a rescue that never runs bounds nothing")

	resolved.Rescue.Enabled = true
	require.Equal(t, []string{"rescue.budget.perRescue", "rescue.budget.weekly"}, resolved.DeclaredUnenforced())
}
