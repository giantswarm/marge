package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/rules"
	"github.com/giantswarm/marge/internal/slack"
)

// poster sends one team's summary to one channel. The all-teams run takes
// it as an interface so a test drives the whole run without Slack.
type poster interface {
	Post(ctx context.Context, channel, text string) error
}

// teamOutcome is what one team's sweep produced. A skipped team carries
// neither a result nor an error. A swept team carries a result, and an error
// as well when the summary could not be posted.
type teamOutcome struct {
	Team string
	// Skipped says why the team was not swept: it has no policy file, or
	// its policy switches the schedule off. Empty when the team was swept.
	Skipped string
	Result  SweepResult
	Err     error
	// Posted reports that the summary reached the team's channel.
	Posted bool
}

// allTeamsRun holds what every team's sweep needs. The unattended run builds
// one and sweeps each team under it.
type allTeamsRun struct {
	Client *github.Client
	Login  string
	Rules  RulesSource
	Opts   RunOptions
	Slack  poster
	// Out carries the per-team progress lines. The summaries themselves go
	// to Slack.
	Out io.Writer
}

// Run sweeps every team that opted its schedule in, in name order,
// and posts one summary per team that changed something.
//
// A team is swept when it has a policy file and that policy leaves the
// schedule enabled. Every other team is skipped and named in the report. A
// team whose files do not parse fails that team alone: one team's broken
// policy must not stop the day's sweep for every other team.
func (r allTeamsRun) Run(ctx context.Context) ([]teamOutcome, error) {
	loader, err := policyLoader(r.Client)
	if err != nil {
		return nil, err
	}
	teams, err := loader.Teams(ctx)
	if err != nil {
		return nil, err
	}
	if len(teams) == 0 {
		return nil, fmt.Errorf("no team has a policy file in %s/%s: nothing to sweep", loader.Source, policy.PolicyDir)
	}

	catalogue, rulesReport := loadRules(ctx, r.Client, r.Rules)
	reportRules(r.Out, rulesReport)

	outcomes := make([]teamOutcome, 0, len(teams))
	for _, team := range teams {
		if err := ctx.Err(); err != nil {
			return outcomes, nil
		}
		outcome := r.sweepTeam(ctx, loader, team, catalogue, rulesReport)
		r.report(outcome)
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// sweepTeam resolves one team's scope and sweeps it, then posts the summary
// when the run changed something.
func (r allTeamsRun) sweepTeam(ctx context.Context, loader policy.Loader, team string, catalogue *rules.Catalogue, rulesReport *SweepRules) teamOutcome {
	scope, err := loader.TeamScope(ctx, team)
	if err != nil {
		return teamOutcome{Team: team, Err: err}
	}
	resolved := scope.Policies.Base()
	if !resolved.Schedule {
		return teamOutcome{Team: team, Skipped: "the policy switches the schedule off"}
	}

	opts := r.Opts
	opts.Team = team
	opts.Policies = scope.Policies
	opts.Rules = catalogue

	found, err := searchPRs(ctx, r.Client, "", r.Login, scope.Repos)
	if err != nil {
		return teamOutcome{Team: team, Err: fmt.Errorf("searching PRs of team %s: %w", team, err)}
	}
	status, err := processOnceWithStatus(ctx, r.Client, r.Login, found.PRs, opts)
	if err != nil {
		return teamOutcome{Team: team, Err: err}
	}

	outcome := teamOutcome{Team: team, Result: buildSweepResult(status, found.Failed, rulesReport)}
	outcome.Posted, err = r.post(ctx, team, resolved.SlackChannel, outcome.Result)
	if err != nil {
		outcome.Err = err
	}
	return outcome
}

// post renders the summary and sends it. It reports false, and no error,
// both when the run changed nothing and when no channel or no Slack token is
// configured: a sweep that did its work is not a failed run because a chat
// message had nowhere to go.
func (r allTeamsRun) post(ctx context.Context, team, channel string, result SweepResult) (bool, error) {
	text, changed := teamSummary(team, result)
	if !changed || r.Slack == nil {
		return false, nil
	}
	if strings.TrimSpace(channel) == "" {
		_, _ = fmt.Fprintf(r.Out, "team %s changed %d PRs and its policy names no Slack channel\n", team, changedCount(result.Summary))
		return false, nil
	}
	if err := r.Slack.Post(ctx, channel, text); err != nil {
		return false, err
	}
	return true, nil
}

// changedCount counts the PRs the run moved on.
func changedCount(counts SweepSummary) int {
	return counts.Merged + counts.Remedied + counts.Refreshed + counts.Retried
}

// report prints one line per team, so the CronJob's log says what every team
// got without the reader opening Slack.
func (r allTeamsRun) report(outcome teamOutcome) {
	switch {
	case outcome.Err != nil:
		_, _ = fmt.Fprintf(r.Out, "team %s failed: %s\n", outcome.Team, outcome.Err)
	case outcome.Skipped != "":
		_, _ = fmt.Fprintf(r.Out, "team %s skipped: %s\n", outcome.Team, outcome.Skipped)
	default:
		_, _ = fmt.Fprintf(r.Out, "team %s: %d PRs, %s, summary posted: %t\n",
			outcome.Team, outcome.Result.Summary.Total, headline(outcome.Result.Summary), outcome.Posted)
	}
}

// allTeamsError joins the errors of the teams that failed, so the CronJob's
// pod fails when a team did, after every other team has run.
func allTeamsError(outcomes []teamOutcome) error {
	var errs []error
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			errs = append(errs, fmt.Errorf("team %s: %w", outcome.Team, outcome.Err))
		}
	}
	return errors.Join(errs...)
}

// loadSlack returns the poster the environment configures, or nil when it
// configures none. A typed nil in the poster interface is never nil, so the
// nil case is returned explicitly.
func loadSlack() poster {
	client := slack.LoadClient()
	if client == nil {
		return nil
	}
	return client
}

// runAllTeams is what `marge sweep --all-teams` and the daily CronJob run.
// It sweeps every opted-in team and fails only after the last team has had
// its turn.
func runAllTeams(ctx context.Context, client *github.Client, login string, source RulesSource, opts RunOptions, asJSON bool) error {
	opts.Team = ""
	opts.Query = ""
	opts.NoTUI = true
	opts.Quiet = true

	run := allTeamsRun{
		Client: client,
		Login:  login,
		Rules:  source,
		Opts:   opts,
		Slack:  loadSlack(),
		Out:    os.Stderr,
	}
	outcomes, err := run.Run(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(allTeamsResult(outcomes)); err != nil {
			return err
		}
	}
	return allTeamsError(outcomes)
}

// AllTeamsTeam is one team's place in the JSON output of an all-teams run.
type AllTeamsTeam struct {
	Team string `json:"team"`
	// Skipped says why the team was not swept; Error why its sweep failed.
	// Result is absent for both.
	Skipped string       `json:"skipped,omitempty"`
	Error   string       `json:"error,omitempty"`
	Posted  bool         `json:"summary_posted"`
	Result  *SweepResult `json:"result,omitempty"`
}

// allTeamsResult turns the outcomes into the JSON --output json prints.
func allTeamsResult(outcomes []teamOutcome) []AllTeamsTeam {
	teams := make([]AllTeamsTeam, 0, len(outcomes))
	for _, outcome := range outcomes {
		entry := AllTeamsTeam{Team: outcome.Team, Skipped: outcome.Skipped, Posted: outcome.Posted}
		if outcome.Err != nil {
			entry.Error = outcome.Err.Error()
		}
		if outcome.Skipped == "" && outcome.Err == nil {
			result := outcome.Result
			entry.Result = &result
		}
		teams = append(teams, entry)
	}
	return teams
}
