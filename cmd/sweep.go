package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/process"
	"github.com/giantswarm/marge/internal/rules"
)

var sweepOpts RunOptions

var sweepFlags struct {
	allTeams     bool
	actions      string
	output       string
	checkTimeout time.Duration
	rulesRepo    string
	rulesRef     string
	rulesPath    string
}

// interactiveCheckTimeout is how long the interactive command waits for
// pending checks on one PR. A sweep waits zero unless --check-timeout asks
// for it: a pending PR is reported and the next sweep decides.
const interactiveCheckTimeout = 5 * time.Minute

func init() {
	sweepCmd.Flags().BoolVar(&sweepFlags.allTeams, "all-teams", false, "Sweep every team whose policy file leaves the schedule enabled; this is what the daily schedule runs")
	sweepCmd.Flags().StringVar(&sweepOpts.Team, "team", "", "Sweep the repositories of this team under its own policy, both read from "+defaultTeamFileRepo+" (or $"+teamFileRepoEnv+")")
	sweepCmd.Flags().StringVar(&sweepOpts.Query, "query", "", "Sweep the bot PRs matching this GitHub search text, the way `marge [query]` does")
	sweepCmd.Flags().StringVar(&sweepFlags.actions, "actions", "", "Comma-separated sweep steps to run, in fixed order: "+strings.Join(process.ActionNames(), ", ")+" (default: all)")
	sweepCmd.Flags().BoolVar(&sweepOpts.DryRun, "dry-run", false, "Show what would be done without making changes")
	sweepCmd.Flags().DurationVar(&sweepFlags.checkTimeout, "check-timeout", 0, "How long to wait for a PR's pending checks; zero reports the PR as waiting")
	sweepCmd.Flags().BoolVarP(&sweepOpts.Watch, "watch", "w", false, "Keep polling for new PRs (every 60s)")
	sweepCmd.Flags().StringVar(&sweepOpts.Org, "org", "", "Limit to repos owned by this org or user (query scope)")
	sweepCmd.Flags().StringVar(&sweepOpts.ReposFile, "repos-file", "", "File with org/repo entries (one per line) to scan for bot PRs instead of searching GitHub (query scope)")
	sweepCmd.Flags().StringSliceVar(&sweepOpts.PRs, "prs", nil, "Sweep only these pull requests of the scope, each a PR URL or owner/repo#number (repeatable, or comma-separated)")
	sweepCmd.Flags().BoolVar(&sweepOpts.NoTUI, "no-tui", false, "Disable live table, print plain-text results instead")
	sweepCmd.Flags().StringVar(&sweepFlags.output, "output", "table", "Output format: table or json")
	sweepCmd.Flags().BoolVar(&sweepOpts.MergeAuto, "merge-auto", false, "Also merge PRs that have auto-merge enabled")
	sweepCmd.Flags().StringVar(&sweepFlags.rulesRepo, "rules-repo", "", "Repository the rule catalogue is read from, as owner/name (default "+rules.DefaultOwner+"/"+rules.DefaultRepo+")")
	sweepCmd.Flags().StringVar(&sweepFlags.rulesRef, "rules-ref", "", "Branch the rule catalogue is read from (default "+rules.DefaultRef+")")
	sweepCmd.Flags().StringVar(&sweepFlags.rulesPath, "rules-path", "", "Read the rule catalogue from this directory instead of the repository")
	sweepCmd.Flags().StringVar(&sweepOpts.SecurityPatterns, "security-patterns", "", "Comma-separated case-insensitive substrings added to the built-in list that flags failing CI checks as security-related")

	rootCmd.AddCommand(sweepCmd)
}

// resolveSweepOptions validates the scope flags and fills the options that
// depend on them.
func resolveSweepOptions(opts *RunOptions) error {
	switch {
	case sweepFlags.allTeams && (opts.Team != "" || opts.Query != "" || opts.Org != "" || opts.ReposFile != "" || len(opts.PRs) > 0):
		return errors.New("--all-teams reads every team's own scope: drop --team, --query, --org, --repos-file and --prs")
	case sweepFlags.allTeams:
	case opts.Team != "" && opts.Query != "":
		return errors.New("--team and --query are mutually exclusive")
	case opts.Team != "" && (opts.Org != "" || opts.ReposFile != ""):
		return errors.New("--org and --repos-file belong to the query scope; drop them with --team")
	case opts.Team == "" && opts.Query == "" && opts.ReposFile == "" && opts.Org == "" && len(opts.PRs) == 0:
		return errors.New("one of --team or --query is required")
	}
	actions, err := process.ParseActions(sweepFlags.actions)
	if err != nil {
		return err
	}
	opts.Actions = actions
	opts.CheckTimeout = sweepFlags.checkTimeout
	switch sweepFlags.output {
	case "table":
	case "json":
		opts.NoTUI = true
		opts.Quiet = true
	default:
		return fmt.Errorf("unknown output %q: use table or json", sweepFlags.output)
	}
	return nil
}

var sweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Sweep a team's bot PRs: classify, approve and merge the eligible green ones",
	Long: `Sweep the open bot PRs of one scope and report every outcome.

Three scopes exist and exactly one is given: --team <name> reads the team's
repositories and the team's policy from giantswarm/github; --query <text>
runs marge's GitHub search the way "marge [query]" does, for personal
repositories and organisations without a team file, under the company
default policy; --all-teams sweeps every team that has a policy file, each
under its own scope and policy, and is what the daily schedule runs. A team
without a policy file and a team whose policy sets "schedule: disabled" are
both skipped and named in the report. One team's unreadable policy fails
that team alone. Only PRs authored by Renovate, Align files, Herald or
Dependabot are touched, never a person's.

The policy is read from the default branch at the start of every sweep:
bot-prs-sweep/default.yaml holds the company defaults, bot-prs-sweep/team-
<name>.yaml one team's deviations, and the botPRsSweep key of a repository
entry that repository's exception. A file that does not parse, or that
names a key or a value the sweep does not know, stops the sweep instead of
applying defaults the team never wrote. Every outcome records the policy it
was decided under.

Each PR gets one bot-prs-sweep/<class> label with its classification.
Green eligible PRs (by company default: patch, minor, digest, pin and
lockfile updates, Align files, Herald) are approved and squash-merged;
majors and unreadable updates are held for a person. A repository whose
exception switches the sweep off receives no write at all, the label
included. A required check that is pending or never reported is a wait,
never a bypass. A failing security check is never merged past. A red
non-required check blocks the merge when it is green on the base head and
is merged past, named in the evidence, when it is red there too.

--actions runs a subset of the steps; --dry-run shows every outcome and
writes nothing. The live table shows every PR's outcome and a one-line
summary follows it.

A failing PR whose head is behind its base branch and whose every failing
check is green on the base branch head is reported as "Stale" instead of
"Failed": the failure was most likely fixed on the base branch after the
PR's last build. The refresh action updates such branches from their base
(the "Update branch" button) so CI re-runs, and reports them as
"Refreshed"; PRs carrying a fresh ai-rescue marker are left alone.

A failing PR whose every failing check is a CircleCI build that CircleCI
itself auto-cancelled (a newer pipeline on the branch, a redundant workflow)
is reported as "Cancelled" instead of "Failed": there is no verdict on the
code yet. The retry action reruns the workflow those builds belong to from
its failed jobs, so the jobs the cancel left blocked run too, and reports
the PR as "Retried". A build with no failed job to rerun from falls back to
the single-build retry. Private CircleCI projects need a token
(CIRCLECI_CLI_TOKEN or ~/.circleci/cli.yml); without one the build cannot be
inspected and the PR stays "Failed", annotated.

A failing check that established nothing about the code is not a failure
either. A cancelled job and a CircleCI pipeline refused because setup
workflows are disabled for the repository are both reported as "CI
unavailable (no verdict)", with the remedy in the detail: rerun the job, or
change the project setting. A security check in that shape is never a
security failure.

A failing or conflicted bot PR that a sibling with a higher version of the
same dependency replaces, or whose diff changes nothing that executes -- a
pinned GitHub Actions SHA whose trailing version comment is all that moved
-- is reported as "Obsolete" rather than as a failure, and stays out of the
rescue path: it wants closing, not fixing. A green PR still merges, and
marge never closes a PR itself.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		if err := resolveSweepOptions(&sweepOpts); err != nil {
			return err
		}

		client, err := gh.NewClient(ctx)
		if err != nil {
			return err
		}

		login, err := gh.AuthenticatedLogin(ctx, client)
		if err != nil {
			return err
		}

		source := RulesSource{Repo: sweepFlags.rulesRepo, Ref: sweepFlags.rulesRef, Path: sweepFlags.rulesPath}

		if sweepFlags.allTeams {
			return watchLoop(ctx, sweepOpts.Watch, func(ctx context.Context) error {
				return runAllTeams(ctx, client, login, source, sweepOpts, sweepFlags.output == "json")
			})
		}

		return watchLoop(ctx, sweepOpts.Watch, func(ctx context.Context) error {
			scope, err := sweepOpts.resolveScope(ctx, client)
			if err != nil {
				return err
			}
			opts := sweepOpts
			opts.Policies = scope.Policies

			catalogue, rulesReport := loadRules(ctx, client, source)
			opts.Rules = catalogue
			if sweepFlags.output != "json" {
				reportRules(os.Stderr, rulesReport)
			}

			repos, err := scopeRepos(scope.Repos, opts.PRs)
			if err != nil {
				return err
			}

			found, err := searchPRs(ctx, client, opts.Query, login, repos)
			if err != nil {
				return fmt.Errorf("searching PRs: %w", err)
			}
			prs, err := filterByPRs(filterByOrg(found.PRs, opts.Org), opts.PRs)
			if err != nil {
				return err
			}

			status, err := processOnceWithStatus(ctx, client, login, prs, opts)
			if err != nil {
				return err
			}
			if sweepFlags.output == "json" {
				return json.NewEncoder(os.Stdout).Encode(buildSweepResult(status, found.Failed, rulesReport))
			}
			for _, f := range found.Failed {
				fmt.Fprintf(os.Stderr, "repository %s not listed: %s\n", f.Repo, f.Err)
			}
			return nil
		})
	},
}
