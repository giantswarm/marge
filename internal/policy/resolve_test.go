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
schedule: enabled
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

	// The exception restricts the update types of the kinds that name a
	// version and switches the rescues off; nothing else changes.
	restricted := set.For("muster")
	require.True(t, restricted.Sweep)
	require.False(t, restricted.Rescue.Enabled)
	require.Equal(t, 3, restricted.Rescue.Weekly)
	require.True(t, restricted.Eligible(pr.KindRenovate, pr.UpdatePatch))
	require.False(t, restricted.Eligible(pr.KindRenovate, pr.UpdateMinor))
	require.False(t, restricted.Eligible(pr.KindDependabot, pr.UpdateMinor))
	require.True(t, restricted.Eligible(pr.KindHerald, pr.UpdateNone),
		"an Align files or Herald PR names no version, so an update type list does not reach it")
	require.True(t, restricted.Eligible(pr.KindAlignFiles, pr.UpdateNone))

	// The exception switches the sweep off for the repository alone.
	require.False(t, set.For("klaus").Sweep)
	require.True(t, set.For("marge").Sweep)

	// Matching is case-insensitive, the way GitHub matches repository names.
	require.False(t, set.For("KLAUS").Sweep)

	// Both shapes of a repository name reach the same exception, so a
	// caller that holds the owner/name of Scope.Repos never silently falls
	// back to the base policy.
	require.False(t, set.For("giantswarm/klaus").Sweep)
	require.True(t, set.For("giantswarm/marge").Sweep)

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
	require.Equal(t, []string{"built-in company defaults"}, resolved.Sources)
}

// TestResolve_scheduleKey proves a team's only switch: the schedule runs
// where the team file says so and nowhere else.
func TestResolve_scheduleKey(t *testing.T) {
	running, err := ParseDocument(TeamFile("shield"), "schedule: enabled\n")
	require.NoError(t, err)
	set, err := NewSet([]File{{Path: TeamFile("shield"), Doc: running}}, nil)
	require.NoError(t, err)
	require.True(t, set.For("any").Schedule)

	silent, err := ParseDocument(TeamFile("shield"), "slackChannel: team-shield\n")
	require.NoError(t, err)
	set, err = NewSet([]File{{Path: TeamFile("shield"), Doc: silent}}, nil)
	require.NoError(t, err)
	require.False(t, set.For("any").Schedule, "a team file that says nothing keeps the company default")
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

	defaults, err := ParseDocument(DefaultFile, companyDefaultFile)
	require.NoError(t, err)
	_, err = NewSet([]File{{Path: DefaultFile, Doc: defaults}}, map[string]Exception{
		"marge": {UpdateTypes: []string{"major"}},
	})
	require.ErrorContains(t, err, "the team merges no major update")
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

// TestResolve_emptyUpdateTypeListNarrowsToNothing separates the two ways a
// file can say nothing about the update types. An absent key leaves the
// lists of the file before it alone. An empty list is a list, and it
// narrows to nothing: the repository or the kind merges no PR. The resolved
// document holds that difference as nil against empty, so both readings
// survive the trip through ParseDocument.
func TestResolve_emptyUpdateTypeListNarrowsToNothing(t *testing.T) {
	defaults, err := ParseDocument(DefaultFile, "updateTypes:\n  renovate: [patch, minor]\n")
	require.NoError(t, err)

	// An absent key on the team file keeps what the default file said.
	silent, err := ParseDocument(TeamFile("shield"), "schedule: enabled\n")
	require.NoError(t, err)
	set, err := NewSet([]File{{Path: DefaultFile, Doc: defaults}, {Path: TeamFile("shield"), Doc: silent}}, nil)
	require.NoError(t, err)
	require.True(t, set.For("cluster-aws").Eligible(pr.KindRenovate, pr.UpdatePatch))

	// An empty list on the team file merges no Renovate PR at all.
	empty, err := ParseDocument(TeamFile("shield"), "updateTypes:\n  renovate: []\n")
	require.NoError(t, err)
	set, err = NewSet([]File{{Path: DefaultFile, Doc: defaults}, {Path: TeamFile("shield"), Doc: empty}}, nil)
	require.NoError(t, err)
	resolved := set.For("cluster-aws")
	require.False(t, resolved.Eligible(pr.KindRenovate, pr.UpdatePatch))
	require.True(t, resolved.Eligible(pr.KindHerald, pr.UpdateNone), "only the kind the file named changes")

	// The same difference on a repository exception.
	set, err = NewSet([]File{{Path: DefaultFile, Doc: defaults}}, map[string]Exception{
		"absent": {},
		"none":   {UpdateTypes: []string{}},
	})
	require.NoError(t, err)
	require.True(t, set.For("absent").Eligible(pr.KindRenovate, pr.UpdatePatch), "an exception without the key narrows nothing")
	require.False(t, set.For("none").Eligible(pr.KindRenovate, pr.UpdatePatch))
	require.True(t, set.For("none").Eligible(pr.KindHerald, pr.UpdateNone), "an exception reaches only the kinds that name a version")
}

// TestResolve_documentReuseDoesNotShare applies one parsed document to two
// sets. The resolved lists live on the document, so each set must hold its
// own copy and one set's exception must never reach the other.
func TestResolve_documentReuseDoesNotShare(t *testing.T) {
	doc, err := ParseDocument(DefaultFile, "updateTypes:\n  renovate: [patch, minor]\n")
	require.NoError(t, err)

	first, err := NewSet([]File{{Path: DefaultFile, Doc: doc}}, map[string]Exception{
		"marge": {UpdateTypes: []string{"patch"}},
	})
	require.NoError(t, err)
	second, err := NewSet([]File{{Path: DefaultFile, Doc: doc}}, nil)
	require.NoError(t, err)

	require.False(t, first.For("marge").Eligible(pr.KindRenovate, pr.UpdateMinor))
	require.True(t, first.Base().Eligible(pr.KindRenovate, pr.UpdateMinor), "the exception stays on its repository")
	require.True(t, second.For("marge").Eligible(pr.KindRenovate, pr.UpdateMinor), "a second set is untouched")
}

// TestNewSet_refusesAnUnresolvedDocument keeps the one construction path.
// A Document's fields are exported for the YAML decoder, so a caller can
// fill them by hand and reach a document whose names were never checked;
// applying it would drop every update type the file names.
func TestNewSet_refusesAnUnresolvedDocument(t *testing.T) {
	byHand := &Document{UpdateTypes: map[string][]string{"renovate": {"patch"}}}

	_, err := NewSet([]File{{Path: DefaultFile, Doc: byHand}}, nil)
	require.ErrorContains(t, err, "only ParseDocument builds a document")
	require.ErrorContains(t, err, DefaultFile)
}
