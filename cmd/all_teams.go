package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v93/github"

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
// as well when its summary was refused.
type teamOutcome struct {
	Team string
	// Skipped says why the team was not swept. Empty when the team was
	// swept.
	Skipped string
	Result  SweepResult
	Err     error
	// Posted reports that the summary reached the team's channel.
	Posted bool
	// Unposted is a summary the run rendered and could not post. The log
	// carries it instead. One the gateway was unavailable for fails no run:
	// the sweep's outcome is its PR actions, which are done by then. One the
	// gateway refused is Err as well, since a refused credential or channel
	// is a fault of the configuration that every later run repeats.
	Unposted *unpostedSummary
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
	// Stdout carries each team's plain results; nil is the process's own.
	Stdout io.Writer
	// Out carries the per-team progress lines. The summaries themselves go
	// to the team channels.
	Out io.Writer
}

// Run sweeps the teams of the run and posts one summary per team that
// changed something. Teams run Opts.Parallel at once, except teams that own
// a common repository, which run one after another (forEachTeam). The
// outcomes, and what each team prints, come in the order the teams were
// named, whatever order they finished in.
//
// A team whose files do not parse fails that team alone: one team's broken
// policy must not stop the sweep for every other team.
func (r teamsRun) Run(ctx context.Context) ([]teamOutcome, error) {
	loader, err := policyLoader(r.Client)
	if err != nil {
		return nil, err
	}
	teams := r.Teams

	catalogue, rulesReport := loadRules(ctx, r.Client, r.Rules, r.Opts.Parallel)
	reportRules(r.Out, rulesReport)

	// Which repositories each team owns decides which teams may run at
	// once, so every scope is resolved before the first sweep starts.
	scopes := resolveTeamScopes(ctx, loader, teams, r.Opts.Parallel)

	outcomes := make([]teamOutcome, len(teams))
	printed := newOrderedOutput(len(teams), r.stdout(), r.Out)
	forEachTeam(scopes, r.Opts.Parallel, func(index int) {
		out := printed.team(index)
		team := r
		team.Opts.Stdout, team.Opts.Stderr, team.Out = &out.stdout, &out.stderr, &out.stderr
		if ctx.Err() != nil {
			// A team the run never reached has no outcome of its own, and
			// an outcome nobody records reads as a team the run covered.
			outcomes[index] = teamOutcome{Team: teams[index], Stopped: true, Skipped: "the run was stopped before this team had its turn"}
		} else {
			outcomes[index] = team.sweepTeam(ctx, loader, scopes[index], catalogue, rulesReport)
		}
		team.report(outcomes[index])
		printed.done(index)
	})
	return outcomes, nil
}

// sweepTeam sweeps one team under its resolved scope, then posts the summary
// when the run changed something.
func (r teamsRun) sweepTeam(ctx context.Context, loader policy.Loader, scoped teamScope, catalogue *rules.Catalogue, rulesReport *SweepRules) teamOutcome {
	team, scope := scoped.team, scoped.scope
	if scoped.err != nil {
		return teamOutcome{Team: team, Stopped: ctx.Err() != nil, Err: scoped.err}
	}
	// The channel is read before the sweep, under the context a stop has
	// not cancelled yet. A channel file the policy needs and the team does
	// not have fails the post alone: the sweep still runs.
	channel, channelErr := r.summaryChannel(ctx, loader, team, scope.Policies.Base().Summary)

	opts := r.Opts
	opts.Team = team
	opts.useScope(scope)
	opts.Rules = catalogue

	found, err := searchPRs(ctx, r.Client, "", r.Login, scope.Repos, r.Opts.Parallel)
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
	if channelErr != nil {
		outcome.Err = channelErr
		return outcome
	}
	return r.postOutcome(ctx, outcome, channel)
}

func (r teamsRun) stdout() io.Writer {
	if r.Stdout == nil {
		return os.Stdout
	}
	return r.Stdout
}

// orderedOutput passes on what each team of a run prints in the order the
// teams were named. The first team not yet done writes straight through, so
// a run of one team, or of one team at a time, prints as it goes; a team
// behind it is held until every team before it is done.
type orderedOutput struct {
	stdout, stderr io.Writer

	mu    sync.Mutex
	teams []*teamOutput
	next  int
}

// teamOutput is what one team prints: its results on stdout, its progress
// and report lines on stderr, and what is held of them until its turn.
type teamOutput struct {
	stdout, stderr teamWriter
	finished       bool
}

func newOrderedOutput(n int, stdout, stderr io.Writer) *orderedOutput {
	o := &orderedOutput{stdout: stdout, stderr: stderr, teams: make([]*teamOutput, n)}
	for i := range o.teams {
		o.teams[i] = &teamOutput{
			stdout: teamWriter{o: o, index: i, dest: stdout},
			stderr: teamWriter{o: o, index: i, dest: stderr},
		}
	}
	return o
}

// team returns the writers of the team at index.
func (o *orderedOutput) team(index int) *teamOutput { return o.teams[index] }

// done records that the team at index has printed everything, and passes on
// what the teams after it hold, up to the first that is not done yet, which
// writes straight through from then on.
func (o *orderedOutput) done(index int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.teams[index].finished = true
	for o.next < len(o.teams) && o.teams[o.next].finished {
		o.next++
		if o.next < len(o.teams) {
			o.teams[o.next].stdout.flush()
			o.teams[o.next].stderr.flush()
		}
	}
}

// teamWriter is one stream of one team.
type teamWriter struct {
	o     *orderedOutput
	index int
	dest  io.Writer
	held  bytes.Buffer
}

func (w *teamWriter) Write(p []byte) (int, error) {
	w.o.mu.Lock()
	defer w.o.mu.Unlock()
	if w.o.next == w.index {
		return w.dest.Write(p)
	}
	return w.held.Write(p)
}

// flush passes on what the stream held. The caller holds the lock.
func (w *teamWriter) flush() {
	_, _ = w.held.WriteTo(w.dest)
}

// postOutcome posts the summary of a swept team and records how that went.
func (r teamsRun) postOutcome(ctx context.Context, outcome teamOutcome, channel string) teamOutcome {
	posted, err := r.post(ctx, outcome.Team, channel, outcome.Result, outcome.Stopped)
	outcome.Posted = posted
	errors.As(err, &outcome.Unposted)
	if err != nil && !notify.Unavailable(err) {
		outcome.Err = err
	}
	return outcome
}

// unpostedSummary is a summary whose post failed, kept so the log can carry
// what the channel did not get.
type unpostedSummary struct {
	Text string
	Err  error
}

func (u *unpostedSummary) Error() string { return u.Err.Error() }
func (u *unpostedSummary) Unwrap() error { return u.Err }

// summaryChannel returns the channel the team's summary goes to: the
// notices channel of its channel file when its policy sets summary, and
// none otherwise. A run without a poster posts nothing, so it reads no file.
func (r teamsRun) summaryChannel(ctx context.Context, loader policy.Loader, team string, summary bool) (string, error) {
	if r.Notices == nil || !summary {
		return "", nil
	}
	channels, err := loader.Channels(ctx, team)
	if err != nil {
		return "", fmt.Errorf("posting the summary of team %s: %w", team, err)
	}
	return channels.Notices.ID, nil
}

// stopNoticeTimeout bounds the notice a stopped run sends. The pod is
// already terminating, so the notice has the termination grace period to
// reach the gateway and SIGKILL follows it.
const stopNoticeTimeout = 10 * time.Second

// post renders the summary and sends it. It reports false, and no error,
// both when the run changed nothing and when the policy posts no summary or
// no gateway is configured: a sweep that did its work is not a failed run
// because a team asked for no chat message.
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
		return false, &unpostedSummary{Text: text, Err: err}
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
	r.reportUnposted(outcome)
}

// reportUnposted logs the error of a summary that was not posted and the
// summary itself, so the run's log holds what the team channel missed.
func (r teamsRun) reportUnposted(outcome teamOutcome) {
	unposted := outcome.Unposted
	if unposted == nil {
		return
	}
	verdict := "the gateway was unavailable; the sweep's work is done, so the run does not fail on it"
	if outcome.Err != nil {
		verdict = "the gateway refused it, which no retry repairs"
	}
	_, _ = fmt.Fprintf(r.Out, "error: team %s: the summary was not posted (%s): %s\n", outcome.Team, verdict, unposted.Err)
	_, _ = fmt.Fprintf(r.Out, "team %s: the summary that was not posted:\n", outcome.Team)
	for _, line := range strings.Split(unposted.Text, "\n") {
		_, _ = fmt.Fprintf(r.Out, "  | %s\n", line)
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
	opts.shareSuites()

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
	// SummaryError says why the summary was not posted.
	SummaryError string `json:"summary_error,omitempty"`
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
		if outcome.Unposted != nil {
			entry.SummaryError = outcome.Unposted.Err.Error()
		}
		if outcome.Skipped == "" && outcome.Err == nil {
			result := outcome.Result
			entry.Result = &result
		}
		teams = append(teams, entry)
	}
	return teams
}
