package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/remedy"
)

// recordingPoster stands in for Slack and keeps every message, so the run is
// tested without a Slack app.
type recordingPoster struct {
	posts []struct{ channel, text string }
	err   error
}

func (p *recordingPoster) Post(_ context.Context, channel, text string) error {
	if p.err != nil {
		return p.err
	}
	p.posts = append(p.posts, struct{ channel, text string }{channel, text})
	return nil
}

// TestAllTeams_sweepsOnlyTheTeamsThatOptedIn is the acceptance criterion: a
// team with no policy file and a team whose policy disables the schedule are
// both skipped, and only the remaining team is swept.
func TestAllTeams_sweepsOnlyTheTeamsThatOptedIn(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"bot-prs-sweep/default.yaml":        "schedule: disabled\n",
		"bot-prs-sweep/team-bumblebee.yaml": "schedule: enabled\nslackChannel: team-bumblebee\n",
		"bot-prs-sweep/team-atlas.yaml":     "schedule: disabled\n",
		"repositories/team-bumblebee.yaml":  "- name: marge\n",
		"repositories/team-atlas.yaml":      "- name: atlas\n",
		"repositories/team-phoenix.yaml":    "- name: phoenix\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	var out bytes.Buffer
	poster := &recordingPoster{}

	outcomes, err := allTeamsRun{
		Client: client,
		Login:  "giantswarm-marge[bot]",
		Rules:  RulesSource{Path: t.TempDir()},
		Opts:   RunOptions{DryRun: true, Quiet: true, NoTUI: true},
		Slack:  poster,
		Out:    &out,
	}.Run(t.Context())
	require.NoError(t, err)
	require.NoError(t, allTeamsError(outcomes))

	byTeam := make(map[string]teamOutcome, len(outcomes))
	for _, outcome := range outcomes {
		byTeam[outcome.Team] = outcome
	}
	require.Len(t, byTeam, 2, "team-phoenix has no policy file and is not a team of the schedule")
	require.NotContains(t, byTeam, "phoenix")
	require.Equal(t, "the policy switches the schedule off", byTeam["atlas"].Skipped)
	require.Empty(t, byTeam["bumblebee"].Skipped)

	require.Contains(t, out.String(), "team atlas skipped")
	require.Empty(t, poster.posts, "the run changed nothing, so the channel stays quiet")
}

// TestAllTeams_oneTeamsBrokenPolicyDoesNotStopTheRest holds the rule the
// daily run depends on: a policy nobody can read fails its own team alone,
// and the pod still fails so the failure is visible.
func TestAllTeams_oneTeamsBrokenPolicyDoesNotStopTheRest(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"bot-prs-sweep/default.yaml":        "schedule: disabled\n",
		"bot-prs-sweep/team-bumblebee.yaml": "schedule: enabled\n",
		"bot-prs-sweep/team-atlas.yaml":     "schedule: enabled\nnotAKey: true\n",
		"repositories/team-bumblebee.yaml":  "- name: marge\n",
		"repositories/team-atlas.yaml":      "- name: atlas\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	var out bytes.Buffer

	outcomes, err := allTeamsRun{
		Client: client,
		Login:  "giantswarm-marge[bot]",
		Rules:  RulesSource{Path: t.TempDir()},
		Opts:   RunOptions{DryRun: true, Quiet: true, NoTUI: true},
		Out:    &out,
	}.Run(t.Context())
	require.NoError(t, err)

	joined := allTeamsError(outcomes)
	require.ErrorContains(t, joined, "team atlas")
	require.ErrorContains(t, joined, "notAKey")

	byTeam := make(map[string]teamOutcome, len(outcomes))
	for _, outcome := range outcomes {
		byTeam[outcome.Team] = outcome
	}
	require.NoError(t, byTeam["bumblebee"].Err, "bumblebee ran although atlas could not")
}

// TestTeamSummary_silentWhenNothingChanged holds the contract that keeps the
// channel worth reading.
func TestTeamSummary_silentWhenNothingChanged(t *testing.T) {
	result := SweepResult{Summary: SweepSummary{Total: 12, Waiting: 4, Skipped: 8}}

	text, changed := teamSummary("bumblebee", result)
	require.False(t, changed)
	require.Empty(t, text)
}

// TestTeamSummary_namesWhatChangedAndWhatIsBlocked renders the sections the
// policy promises: merged, remedied, blocked with links, and the skipped
// count.
func TestTeamSummary_namesWhatChangedAndWhatIsBlocked(t *testing.T) {
	result := SweepResult{
		Summary: SweepSummary{Total: 5, Merged: 1, Remedied: 1, Failed: 1, Skipped: 2},
		Merged: []SweepPREntry{{
			Owner: "giantswarm", Repo: "marge", Number: 111,
			URL: "https://github.com/giantswarm/marge/pull/111", Detail: "squash-merged",
		}},
		Remedied: []SweepPREntry{{
			Owner: "giantswarm", Repo: "muster", Number: 22,
			URL: "https://github.com/giantswarm/muster/pull/22", Detail: "reran the failed jobs",
		}},
		ActionRequired: []SweepPREntry{{
			Owner: "giantswarm", Repo: "klaus", Number: 7,
			URL: "https://github.com/giantswarm/klaus/pull/7", Detail: "unit tests failed twice on the same commit",
		}},
	}

	text, changed := teamSummary("bumblebee", result)
	require.True(t, changed)
	require.Contains(t, text, "*bumblebee* swept 5 bot PRs")
	require.Contains(t, text, "1 merged, 1 remedied, 1 blocked, 2 skipped")
	require.Contains(t, text, "<https://github.com/giantswarm/marge/pull/111|giantswarm/marge#111> — squash-merged")
	require.Contains(t, text, "reran the failed jobs")
	require.Contains(t, text, "unit tests failed twice on the same commit")
	require.Contains(t, text, "2 PRs skipped by policy.")
}

// TestTeamSummary_boundsALongSection keeps one team's bad day from filling
// the channel.
func TestTeamSummary_boundsALongSection(t *testing.T) {
	result := SweepResult{Summary: SweepSummary{Total: 30, Merged: 30}}
	for i := range 30 {
		result.Merged = append(result.Merged, SweepPREntry{Owner: "giantswarm", Repo: "marge", Number: i})
	}

	text, changed := teamSummary("bumblebee", result)
	require.True(t, changed)
	require.Contains(t, text, "and 20 more")
}

// TestDailyActionsWriteNoCode is the acceptance criterion for the daily
// schedule: every action a rule may name acts through the GitHub or CircleCI
// API, and none of them pushes a commit of its own. A new action that writes
// code fails this test, and the schedule's action set has to be narrowed
// before it merges.
func TestDailyActionsWriteNoCode(t *testing.T) {
	apiOnly := []remedy.Name{
		remedy.CircleCIRetry,
		remedy.Close,
		remedy.DispatchAlignWorkflow,
		remedy.FixProtectionContext,
		remedy.MarkWait,
		remedy.RerunFailed,
		remedy.StrictChain,
		remedy.UpdateBranch,
	}
	require.Equal(t, apiOnly, remedy.Default().Names())
}
