package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/notify"
	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/rules"
)

// poster sends one team's summary to one channel. The run takes it as an
// interface so a test drives the whole run without the gateway.
type poster interface {
	Post(ctx context.Context, team, channel, text string) error
}

// teamOutcome is what one team's sweep produced. A skipped team carries
// neither a result nor an error. A swept team carries a result, and an error
// as well when the summary could not be posted.
type teamOutcome struct {
	Team string
	// Skipped says why the team was not swept. Empty when the team was
	// swept.
	Skipped string
	Result  SweepResult
	Err     error
	// Posted reports that the summary reached the team's channel.
	Posted bool
	// Stopped says the run's context was cancelled before the team was
	// finished. On a scheduled run that is Kubernetes at
	// activeDeadlineSeconds, and the counts of Result cover only the PRs
	// the run reached.
	Stopped bool
}

// teamsRun holds what one sweep of several teams needs. The unattended run
// builds one and sweeps each team under it, and so does a manual run that
// names more than one team.
type teamsRun struct {
	Client *github.Client
	Login  string
	Rules  RulesSource
	Opts   RunOptions
	// Teams are the teams an operator named. Empty means the schedule's
	// own run, which sweeps every team that has a policy file and skips
	// the teams whose policy switches the schedule off.
	Teams   []string
	Notices poster
	// Out carries the per-team progress lines. The summaries themselves go
	// to the team channels.
	Out io.Writer
}

// Run sweeps the teams of the run, in name order, and posts one summary per
// team that changed something.
//
// A team whose files do not parse fails that team alone: one team's broken
// policy must not stop the sweep for every other team.
func (r teamsRun) Run(ctx context.Context) ([]teamOutcome, error) {
	loader, err := policyLoader(r.Client)
	if err != nil {
		return nil, err
	}
	teams := r.Teams

	catalogue, rulesReport := loadRules(ctx, r.Client, r.Rules)
	reportRules(r.Out, rulesReport)

	outcomes := make([]teamOutcome, 0, len(teams))
	for i, team := range teams {
		if ctx.Err() != nil {
			// The teams left have no outcome of their own, and an
			// outcome nobody records reads as a team the run covered.
			for _, left := range teams[i:] {
				outcome := teamOutcome{Team: left, Stopped: true, Skipped: "the run was stopped before this team had its turn"}
				r.report(outcome)
				outcomes = append(outcomes, outcome)
			}
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
func (r teamsRun) sweepTeam(ctx context.Context, loader policy.Loader, team string, catalogue *rules.Catalogue, rulesReport *SweepRules) teamOutcome {
	scope, err := loader.TeamScope(ctx, team)
	if err != nil {
		return teamOutcome{Team: team, Stopped: ctx.Err() != nil, Err: err}
	}
	resolved := scope.Policies.Base()

	opts := r.Opts
	opts.Team = team
	opts.Policies = scope.Policies
	opts.Rules = catalogue

	found, err := searchPRs(ctx, r.Client, "", r.Login, scope.Repos)
	if err != nil && ctx.Err() == nil {
		return teamOutcome{Team: team, Err: fmt.Errorf("searching PRs of team %s: %w", team, err)}
	}
	status, err := processOnceWithStatus(ctx, r.Client, r.Login, found.PRs, opts)
	if err != nil {
		return teamOutcome{Team: team, Stopped: ctx.Err() != nil, Err: err}
	}

	outcome := teamOutcome{
		Team:    team,
		Stopped: ctx.Err() != nil,
		Result:  buildSweepResult(status, found.Failed, rulesReport),
	}
	outcome.Posted, err = r.post(ctx, team, resolved.SlackChannel, outcome.Result, outcome.Stopped)
	if err != nil {
		outcome.Err = err
	}
	return outcome
}

// stopNoticeTimeout bounds the notice a stopped run sends. The pod is
// already terminating, so the notice has the termination grace period to
// reach the gateway and SIGKILL follows it.
const stopNoticeTimeout = 10 * time.Second

// post renders the summary and sends it. It reports false, and no error,
// both when the run changed nothing and when no channel or no gateway is
// configured: a sweep that did its work is not a failed run because a chat
// message had nowhere to go.
//
// A stopped run posts whatever it reached, changed or not, under a context
// of its own: the one it was swept under is cancelled, and the notice is
// the only trace of the stop outside the cluster.
func (r teamsRun) post(ctx context.Context, team, channel string, result SweepResult, stopped bool) (bool, error) {
	text, changed := teamSummary(team, result)
	switch {
	case stopped:
		text = stoppedSummary(team, result)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), stopNoticeTimeout)
		defer cancel()
	case !changed:
		return false, nil
	}
	if r.Notices == nil || strings.TrimSpace(channel) == "" {
		return false, nil
	}
	if err := r.Notices.Post(ctx, team, channel, text); err != nil {
		return false, err
	}
	return true, nil
}

// report prints one line per team, so the run's log says what every team
// got without the reader opening Slack, and then the reason behind each PR
// the run did not move on. A count alone cannot tell a blocked PR from an
// approval the write-access guard refused, and those need different repairs.
func (r teamsRun) report(outcome teamOutcome) {
	switch {
	case outcome.Err != nil && outcome.Stopped:
		_, _ = fmt.Fprintf(r.Out, "team %s was stopped before it finished: %s\n", outcome.Team, outcome.Err)
	case outcome.Err != nil:
		_, _ = fmt.Fprintf(r.Out, "team %s failed: %s\n", outcome.Team, outcome.Err)
	case outcome.Skipped != "":
		_, _ = fmt.Fprintf(r.Out, "team %s skipped: %s\n", outcome.Team, outcome.Skipped)
	case outcome.Stopped:
		_, _ = fmt.Fprintf(r.Out, "team %s was stopped before it finished: %d PRs, %s, summary posted: %t\n",
			outcome.Team, outcome.Result.Summary.Total, headline(outcome.Result.Summary), outcome.Posted)
		r.reportReasons(outcome.Result)
	default:
		_, _ = fmt.Fprintf(r.Out, "team %s: %d PRs, %s, summary posted: %t\n",
			outcome.Team, outcome.Result.Summary.Total, headline(outcome.Result.Summary), outcome.Posted)
		r.reportReasons(outcome.Result)
	}
}

// reportReasons prints why the run left each PR where it is, and which
// repositories it could not read.
func (r teamsRun) reportReasons(result SweepResult) {
	blocked := make([]SweepPREntry, 0, len(result.SecurityFailures)+len(result.ActionRequired))
	blocked = append(blocked, result.SecurityFailures...)
	blocked = append(blocked, result.ActionRequired...)

	reportEntries(r.Out, "blocked", blocked)
	reportEntries(r.Out, "skipped", result.Skipped)
	for _, failure := range result.RepositoriesFailed {
		_, _ = fmt.Fprintf(r.Out, "  repository %s could not be listed: %s\n", failure.Repo, failure.Error)
	}
}

// reportEntries names each pull request of one section with the detail that
// explains its outcome. Every entry is printed: the team's summary line comes
// first and a log holds the rest, which a Slack channel does not.
func reportEntries(w io.Writer, label string, entries []SweepPREntry) {
	for _, entry := range entries {
		_, _ = fmt.Fprintf(w, "  %s %s/%s#%d: %s\n", label, entry.Owner, entry.Repo, entry.Number, logDetail(entry))
	}
}

// logDetail is the reason an entry carries, or a stand-in when it carries
// none, so a line never reads as if the reason were lost.
func logDetail(entry SweepPREntry) string {
	if detail := strings.TrimSpace(entry.Detail); detail != "" {
		return detail
	}
	return entry.Status
}

// teamsError joins the errors of the teams that failed, so the CronJob's
// pod fails when a team did, after every other team has run.
func teamsError(outcomes []teamOutcome) error {
	var errs []error
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			errs = append(errs, fmt.Errorf("team %s: %w", outcome.Team, outcome.Err))
		}
	}
	return errors.Join(errs...)
}

// loadNotices returns the poster the environment configures, or nil when it
// configures none. A typed nil in the poster interface is never nil, so the
// nil case is returned explicitly.
func loadNotices() poster {
	client := notify.LoadClient()
	if client == nil {
		return nil
	}
	return client
}

// runTeams is what `marge sweep --team` runs when it names more than one
// team, and what one named team runs under --post-summary. Each team is
// swept under its own scope and policy, and the teams are reported together.
// A summary reaches the team's channel only when post says so: a sweep by
// hand posts to no team channel, whether it names one team or five.
func runTeams(ctx context.Context, client *github.Client, login string, source RulesSource, opts RunOptions, teams []string, post, asJSON bool) error {
	var notices poster
	if post {
		notices = loadNotices()
	}
	return runTeamSweeps(ctx, client, login, source, opts, teams, notices, asJSON)
}

// runTeamSweeps sweeps the teams and fails only after the last team has had
// its turn.
func runTeamSweeps(ctx context.Context, client *github.Client, login string, source RulesSource, opts RunOptions, teams []string, notices poster, asJSON bool) error {
	opts.Team = ""
	opts.Query = ""
	// The live table shows one sweep and these are several, so the run
	// prints plain results instead. It stays quiet only for JSON, where
	// stdout carries the result and nothing else.
	opts.NoTUI = true
	opts.Quiet = asJSON

	run := teamsRun{
		Client:  client,
		Login:   login,
		Rules:   source,
		Opts:    opts,
		Teams:   teams,
		Notices: notices,
		Out:     os.Stderr,
	}
	outcomes, err := run.Run(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(teamsResult(outcomes)); err != nil {
			return err
		}
	}
	return teamsError(outcomes)
}

// TeamSweep is one team's place in the JSON output of a run that sweeps
// several teams.
type TeamSweep struct {
	Team string `json:"team"`
	// Skipped says why the team was not swept; Error why its sweep failed.
	// Result is absent for both.
	Skipped string `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
	Posted  bool   `json:"summary_posted"`
	// Stopped says the run did not finish this team, so Result covers only
	// the PRs it reached.
	Stopped bool         `json:"stopped,omitempty"`
	Result  *SweepResult `json:"result,omitempty"`
}

// teamsResult turns the outcomes into the JSON --output json prints.
func teamsResult(outcomes []teamOutcome) []TeamSweep {
	teams := make([]TeamSweep, 0, len(outcomes))
	for _, outcome := range outcomes {
		entry := TeamSweep{Team: outcome.Team, Skipped: outcome.Skipped, Posted: outcome.Posted, Stopped: outcome.Stopped}
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
