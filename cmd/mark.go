package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
)

var markOpts struct {
	Outcome string
	Reason  string
	Tool    string
}

// validMarkOutcomes are the rescue outcomes a marker may record. "failed"
// means a rescue was attempted on the current code and could not fix it;
// "blocked" means the fix is known but waits on something external
// (upstream release, ecosystem support, budget).
var validMarkOutcomes = map[string]bool{
	"failed":  true,
	"blocked": true,
}

func init() {
	markCmd.Flags().StringVar(&markOpts.Outcome, "outcome", "failed", "Rescue outcome: \"failed\" or \"blocked\"")
	markCmd.Flags().StringVar(&markOpts.Reason, "reason", "", "Short explanation of why the rescue did not succeed")
	markCmd.Flags().StringVar(&markOpts.Tool, "tool", "ai", "Name of the tool/agent that attempted the rescue (e.g. \"klaus\")")

	rootCmd.AddCommand(markCmd)
}

var markCmd = &cobra.Command{
	Use:   "mark <pr-url>",
	Short: "Record a failed AI rescue attempt on a PR",
	Long: `Post a machine-readable ai-rescue marker comment on a pull request.

Subsequent sweeps read the marker and annotate the PR's failure entry with
the prior rescue outcome, so the operator can tell "needs a first rescue"
apart from "an automated rescue already failed here". The marker records
the PR's current head SHA and a fingerprint of its diff: when the branch
later changes content (a new version, a pushed fix) the marker is reported
as stale and the PR becomes fair game again, while a Renovate rebase that
leaves the diff unchanged keeps it valid.

Any tool that can comment on a PR can write the marker -- this command is
a convenience so callers do not need to know the marker format.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		client, err := gh.NewClient(ctx)
		if err != nil {
			return err
		}

		marker, owner, repo, number, err := markRescue(ctx, client, args[0], markOpts.Outcome, markOpts.Reason, markOpts.Tool)
		if err != nil {
			return err
		}

		fmt.Printf("Marked %s/%s#%d: rescue %s (%s)\n", owner, repo, number, marker.Outcome, pinned(marker))
		return nil
	},
}

// pinned renders what the marker ties the attempt to, for the confirmation
// line: the head SHA and whichever fingerprints could be recorded.
func pinned(m *pr.RescueMarker) string {
	parts := []string{fmt.Sprintf("head %.8s", m.HeadSHA)}
	if m.PatchID != "" {
		parts = append(parts, "patch_id "+m.PatchID)
	}
	if m.ChangeID != "" {
		parts = append(parts, "change_id "+m.ChangeID)
	}
	return strings.Join(parts, ", ")
}

// markRescue posts an ai-rescue marker comment on the PR and returns the
// marker that was written. Shared by the CLI command and the MCP tool.
func markRescue(ctx context.Context, client *github.Client, prURL, outcome, reason, tool string) (*pr.RescueMarker, string, string, int, error) {
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	if !validMarkOutcomes[outcome] {
		return nil, "", "", 0, fmt.Errorf("invalid outcome %q (must be \"failed\" or \"blocked\")", outcome)
	}

	owner, repo, number, err := pr.ParsePRURL(prURL)
	if err != nil {
		return nil, "", "", 0, err
	}

	pullReq, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, "", "", 0, fmt.Errorf("fetching PR: %w", err)
	}

	marker := &pr.RescueMarker{
		Tool:        tool,
		Outcome:     outcome,
		Reason:      reason,
		HeadSHA:     pullReq.GetHead().GetSHA(),
		At:          time.Now().UTC().Truncate(time.Second),
		Fingerprint: process.FingerprintPR(ctx, client, owner, repo, pullReq),
	}

	body := marker.CommentBody()
	_, _, err = client.Issues.CreateComment(ctx, owner, repo, number, github.IssueCommentRequest{Body: body})
	if err != nil {
		return nil, "", "", 0, fmt.Errorf("posting marker comment: %w", err)
	}

	return marker, owner, repo, number, nil
}
