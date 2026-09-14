package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	gh "github.com/teemow/marge/internal/github"
)

var sweepOpts RunOptions

func init() {
	sweepCmd.Flags().BoolVar(&sweepOpts.DryRun, "dry-run", false, "Show what would be done without making changes")
	sweepCmd.Flags().BoolVarP(&sweepOpts.Watch, "watch", "w", false, "Keep polling for new PRs (every 60s)")
	sweepCmd.Flags().StringVar(&sweepOpts.Author, "author", "all", "Filter by PR author: \"renovate\", \"dependabot\", or \"all\"")
	sweepCmd.Flags().StringVar(&sweepOpts.Org, "org", "", "Limit to repos owned by this org or user")
	sweepCmd.Flags().StringVar(&sweepOpts.ReposFile, "repos-file", "", "File with org/repo entries (one per line) to also scan for bot PRs")
	sweepCmd.Flags().BoolVar(&sweepOpts.NoTUI, "no-tui", false, "Disable live table, print plain-text results instead")
	sweepCmd.Flags().BoolVar(&sweepOpts.MergeAuto, "merge-auto", false, "Also merge PRs that have auto-merge enabled")
	sweepCmd.Flags().BoolVar(&sweepOpts.RefreshStale, "refresh-stale", false, "Update the branch of stale PRs (behind base, failing checks green on base) so CI re-runs")
	sweepCmd.Flags().BoolVar(&sweepOpts.RetryCancelled, "retry-cancelled", false, "Retry CircleCI builds that CircleCI auto-cancelled on the PR head so the same commit gets a real verdict")
	sweepCmd.Flags().StringVar(&sweepOpts.TrustedAuthors, "trusted-authors", "renovate[bot],dependabot[bot]", "Comma-separated list of trusted PR author logins")
	sweepCmd.Flags().StringVar(&sweepOpts.SecurityPatterns, "security-patterns", "", "Comma-separated list of case-insensitive substrings used to flag failing CI checks as security-related (defaults to a built-in list)")

	rootCmd.AddCommand(sweepCmd)
}

var sweepCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Merge all dependency update PRs, report failures",
	Long: `Automatically attempt to merge every open Renovate and Dependabot PR
that requests your review. The live table shows every PR's outcome and a
one-line summary follows it; PRs that could not be merged stay in the
table so you can fix them manually.

A failing PR whose head is behind its base branch and whose every failing
check is green on the base branch head is reported as "Stale" instead of
"Failed": the failure was most likely fixed on the base branch after the
PR's last build. With --refresh-stale, marge updates such branches from
their base (the "Update branch" button) so CI re-runs, and reports them as
"Refreshed"; PRs carrying a fresh ai-rescue marker are left alone.

A failing PR whose every failing check is a CircleCI build that CircleCI
itself auto-cancelled (a newer pipeline on the branch, a redundant workflow)
is reported as "Cancelled" instead of "Failed": there is no verdict on the
code yet. With --retry-cancelled, marge retries such builds on the same
commit and reports the PR as "Retried". Private CircleCI projects need a
token (CIRCLECI_CLI_TOKEN or ~/.circleci/cli.yml); without one the build
cannot be inspected and the PR stays "Failed", annotated.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		client, err := gh.NewClient(ctx)
		if err != nil {
			return err
		}

		me, _, err := client.Users.Get(ctx, "")
		if err != nil {
			return fmt.Errorf("getting authenticated user: %w", err)
		}
		login := me.GetLogin()

		return watchLoop(ctx, sweepOpts.Watch, func(ctx context.Context) error {
			prs, err := searchPRs(ctx, client, "", login, sweepOpts.Author, sweepOpts.ReposFile)
			if err != nil {
				return fmt.Errorf("searching PRs: %w", err)
			}

			if sweepOpts.Org != "" {
				filtered := prs[:0]
				for _, p := range prs {
					if strings.EqualFold(p.Owner, sweepOpts.Org) {
						filtered = append(filtered, p)
					}
				}
				prs = filtered
			}

			_, err = processOnceWithStatus(ctx, client, login, prs, sweepOpts)
			return err
		})
	},
}
