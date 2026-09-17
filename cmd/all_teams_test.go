package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
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

// TestTeamsRun_postsOnlyWithAPoster pins where a summary goes. A run
// without --post-summary carries no poster, and a team channel then stays
// quiet whatever the run changed.
func TestTeamsRun_postsOnlyWithAPoster(t *testing.T) {
	result := SweepResult{Summary: SweepSummary{Total: 2, Merged: 2}}

	poster := &recordingPoster{}
	posted, err := teamsRun{Slack: poster}.post(t.Context(), "bumblebee", "team-bumblebee", result)
	require.NoError(t, err)
	require.True(t, posted)
	require.Len(t, poster.posts, 1)
	require.Equal(t, "team-bumblebee", poster.posts[0].channel)
	require.Contains(t, poster.posts[0].text, "bumblebee")

	posted, err = teamsRun{}.post(t.Context(), "bumblebee", "team-bumblebee", result)
	require.NoError(t, err)
	require.False(t, posted, "a run with no poster posts nothing")

	posted, err = teamsRun{Slack: poster}.post(t.Context(), "bumblebee", "", result)
	require.NoError(t, err)
	require.False(t, posted, "a team whose policy names no channel posts nothing")
	require.Len(t, poster.posts, 1)
}

// TestTeamsRun_sweepsTheNamedTeamsInOrder is the acceptance criterion of a
// run of several teams: each one is swept under its own policy, in the
// order given, and a team nobody named is not swept.
func TestTeamsRun_sweepsTheNamedTeamsInOrder(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"bot-prs-sweep/team-bumblebee.yaml": "slackChannel: team-bumblebee\n",
		"bot-prs-sweep/team-atlas.yaml":     "slackChannel: team-atlas\n",
		"repositories/team-bumblebee.yaml":  "- name: marge\n",
		"repositories/team-atlas.yaml":      "- name: atlas\n",
		"repositories/team-phoenix.yaml":    "- name: phoenix\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	var out bytes.Buffer

	outcomes, err := teamsRun{
		Client: client,
		Login:  "giantswarm-marge[bot]",
		Rules:  RulesSource{Path: t.TempDir()},
		Opts:   RunOptions{DryRun: true, Quiet: true, NoTUI: true},
		Teams:  []string{"atlas", "phoenix"},
		Out:    &out,
	}.Run(t.Context())
	require.NoError(t, err)
	require.NoError(t, teamsError(outcomes))

	require.Len(t, outcomes, 2)
	require.Equal(t, "atlas", outcomes[0].Team)
	require.Empty(t, outcomes[0].Skipped, "the operator named atlas, so its schedule key decides nothing")
	require.Equal(t, "phoenix", outcomes[1].Team)
	require.Empty(t, outcomes[1].Skipped, "phoenix has no policy file and is swept under the company defaults")
	require.NotContains(t, out.String(), "bumblebee", "a team nobody named is not swept")
}

// TestTeamsRun_namedTeamWithoutARepositoryListFailsAlone keeps a typo in one
// name from stopping the other teams of the same run.
func TestTeamsRun_namedTeamWithoutARepositoryListFailsAlone(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"repositories/team-atlas.yaml": "- name: atlas\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	var out bytes.Buffer

	outcomes, err := teamsRun{
		Client: client,
		Login:  "giantswarm-marge[bot]",
		Rules:  RulesSource{Path: t.TempDir()},
		Opts:   RunOptions{DryRun: true, Quiet: true, NoTUI: true},
		Teams:  []string{"atals", "atlas"},
		Out:    &out,
	}.Run(t.Context())
	require.NoError(t, err)

	require.ErrorContains(t, teamsError(outcomes), "team atals")
	require.NoError(t, outcomes[1].Err, "atlas ran although the misspelt name could not")
}

// TestTeamsRun_oneTeamsBrokenPolicyDoesNotStopTheRest holds the rule the
// daily run depends on: a policy nobody can read fails its own team alone,
// and the pod still fails so the failure is visible.
func TestTeamsRun_oneTeamsBrokenPolicyDoesNotStopTheRest(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"bot-prs-sweep/team-bumblebee.yaml": "slackChannel: team-bumblebee\n",
		"bot-prs-sweep/team-atlas.yaml":     "notAKey: true\n",
		"repositories/team-bumblebee.yaml":  "- name: marge\n",
		"repositories/team-atlas.yaml":      "- name: atlas\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	var out bytes.Buffer

	outcomes, err := teamsRun{
		Client: client,
		Login:  "giantswarm-marge[bot]",
		Rules:  RulesSource{Path: t.TempDir()},
		Opts:   RunOptions{DryRun: true, Quiet: true, NoTUI: true},
		Teams:  []string{"atlas", "bumblebee"},
		Out:    &out,
	}.Run(t.Context())
	require.NoError(t, err)

	joined := teamsError(outcomes)
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

// TestHeadline_accountsForEveryPR is the rule the CronJob's log depends on:
// the counts add up to the total. A line that drops a category reads as if
// the sweep lost a PR, which is how a real gazelle run reported 12 PRs as
// "3 merged, 3 blocked, 5 skipped".
func TestHeadline_accountsForEveryPR(t *testing.T) {
	counts := SweepSummary{
		Total: 12, Merged: 3, Failed: 2, SecurityFailures: 1, Skipped: 5, Stale: 1,
	}

	line := headline(counts)
	require.Contains(t, line, "3 merged")
	require.Contains(t, line, "3 blocked")
	require.Contains(t, line, "1 stale")
	require.Contains(t, line, "5 skipped")
	require.NotContains(t, line, "other", "every PR is in a named category")
}

// TestHeadline_namesTheRemainder keeps an outcome nobody added to the list
// visible, instead of silently dropping it from the counts.
func TestHeadline_namesTheRemainder(t *testing.T) {
	require.Contains(t, headline(SweepSummary{Total: 12, Merged: 3}), "9 other")
}

// TestHeadline_everyCategoryIsNamed walks each count of SweepSummary and
// fails when one of them is missing from the line, so a new outcome cannot
// be added to the summary and forgotten here.
func TestHeadline_everyCategoryIsNamed(t *testing.T) {
	for _, part := range headlineParts(SweepSummary{}) {
		require.NotEmpty(t, part.name)
	}
	counts := SweepSummary{
		Total: 10, Merged: 1, Remedied: 1, Refreshed: 1, Retried: 1, Failed: 1,
		Stale: 1, Cancelled: 1, Obsolete: 1, Waiting: 1, Skipped: 1,
	}
	require.NotContains(t, headline(counts), "other")
}

// refusingRepoMux serves one renovate PR whose repository grants no write
// access, which is what a GitHub App without contents: write meets.
func refusingRepoMux(t *testing.T) *github.Client {
	t.Helper()
	writeJSON := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"name":"repo","permissions":{"admin":false,"push":false,"pull":true}}`)
	})
	mux.HandleFunc("GET /repos/org/repo/pulls/1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"number":1,"title":"chore(deps): update all non-major dependencies","mergeable_state":"clean","user":{"login":"renovate[bot]"},
			"head":{"sha":"aaa111","ref":"renovate/foo","repo":{"full_name":"org/repo"}},"base":{"sha":"bbb222","ref":"main"}}`)
	})
	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"state":"success","statuses":[{"context":"go-build","state":"success"}]}`)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"total_count":0,"check_runs":[]}`)
	})
	mux.HandleFunc("GET /repos/org/repo/issues/1/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[]`)
	})
	mux.HandleFunc("GET /repos/org/repo/pulls/1/reviews", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[]`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return client
}

// TestReport_namesTheRefusedApproval is the acceptance criterion the gazelle
// run failed: the write-access guard refused every approval and the log said
// "3 blocked" and nothing else. The log must name the repository and the
// reason, so a permission trim is visible without anyone re-running the sweep
// with --output json.
func TestReport_namesTheRefusedApproval(t *testing.T) {
	client := refusingRepoMux(t)

	status, err := processOnceWithStatus(t.Context(), client, "giantswarm-marge[bot]",
		[]pr.PRInfo{{Owner: "org", Repo: "repo", Number: 1}},
		RunOptions{Quiet: true, NoTUI: true})
	require.NoError(t, err)

	var out bytes.Buffer
	teamsRun{Out: &out}.report(teamOutcome{Team: "bumblebee", Result: buildSweepResult(status, nil, nil)})

	line := out.String()
	require.Contains(t, line, "team bumblebee: 1 PRs")
	require.Contains(t, line, "blocked org/repo#1")
	require.Contains(t, line, "approve refused")
	require.Contains(t, line, "no write access to the repository")
}

// TestReport_namesEveryBlockedPR holds the rule that separates the log from
// the Slack summary: the log names every PR, and the team's counts stay
// readable because the summary line comes before them.
func TestReport_namesEveryBlockedPR(t *testing.T) {
	result := SweepResult{Summary: SweepSummary{Total: 30, Failed: 30}}
	for i := range 30 {
		result.ActionRequired = append(result.ActionRequired, SweepPREntry{
			Owner: "giantswarm", Repo: "marge", Number: i, Detail: "unit tests failed",
		})
	}

	var out bytes.Buffer
	teamsRun{Out: &out}.report(teamOutcome{Team: "bumblebee", Result: result})

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.Len(t, lines, 31, "the team line and one line per PR")
	require.Contains(t, lines[0], "team bumblebee: 30 PRs")
	require.Contains(t, lines[30], "blocked giantswarm/marge#29")
}
