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

// markRequest is one marker to write: the rescue outcome on a PR, and what
// the attempt cost and who started it.
type markRequest struct {
	PRRef   string
	Outcome string
	Reason  string
	Tool    string
	DryRun  bool
	// CostUSD is the cost the attempt reported; nil when it reported none.
	CostUSD      *float64
	Duration     time.Duration
	DispatchedBy string
	RunID        string
}

var markOpts struct {
	markRequest
	costUSD float64
}

// validMarkOutcomes are the rescue outcomes a marker may record. "fixed"
// means a fix was pushed and CI runs again; "merged" means the rescue saw
// the PR merge; "failed" means a rescue was attempted on the current code
// and could not fix it; "blocked" means the fix is known but waits on
// something external (upstream release, ecosystem support, budget).
var validMarkOutcomes = map[string]bool{
	pr.RescueOutcomeFixed:   true,
	pr.RescueOutcomeMerged:  true,
	pr.RescueOutcomeFailed:  true,
	pr.RescueOutcomeBlocked: true,
}

func init() {
	markCmd.Flags().StringVar(&markOpts.Outcome, "outcome", "failed", "Rescue outcome: \"fixed\", \"merged\", \"failed\" or \"blocked\"")
	markCmd.Flags().StringVar(&markOpts.Reason, "reason", "", "Short explanation of the outcome")
	markCmd.Flags().StringVar(&markOpts.Tool, "tool", "ai", "Name of the tool/agent that attempted the rescue (e.g. \"klaus\")")
	markCmd.Flags().Float64Var(&markOpts.costUSD, "cost-usd", 0, "Estimated cost of the attempt in US dollars, as the agent reported it; unset records \"cost not reported\"")
	markCmd.Flags().DurationVar(&markOpts.Duration, "duration", 0, "How long the attempt took")
	markCmd.Flags().StringVar(&markOpts.DispatchedBy, "dispatched-by", "", "Who started the run, as the rescue handoff's triggered_by names it")
	markCmd.Flags().StringVar(&markOpts.RunID, "run-id", "", "The run the attempt belongs to")
	markCmd.Flags().BoolVar(&markOpts.DryRun, "dry-run", false, "Show the marker that would be written without posting it")

	rootCmd.AddCommand(markCmd)
}

var markCmd = &cobra.Command{
	Use:   "mark <pr-url>",
	Short: "Record the outcome of an AI rescue attempt on a PR",
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

		req := markOpts.markRequest
		req.PRRef = args[0]
		if cmd.Flags().Changed("cost-usd") {
			req.CostUSD = &markOpts.costUSD
		}
		marker, owner, repo, number, err := markRescue(ctx, client, req)
		if err != nil {
			return err
		}

		verb := "Marked"
		if markOpts.DryRun {
			verb = "dry-run: would mark"
		}
		fmt.Printf("%s %s/%s#%d: rescue %s (%s)\n", verb, owner, repo, number, marker.Outcome, pinned(marker))
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
// marker that was written. Shared by the CLI command and the MCP tool. A dry
// run builds the marker, head SHA and fingerprint included, and posts
// nothing, so a caller sees exactly what the write would say.
func markRescue(ctx context.Context, client *github.Client, req markRequest) (*pr.RescueMarker, string, string, int, error) {
	outcome := strings.ToLower(strings.TrimSpace(req.Outcome))
	if !validMarkOutcomes[outcome] {
		return nil, "", "", 0, fmt.Errorf("invalid outcome %q (must be \"fixed\", \"merged\", \"failed\" or \"blocked\")", outcome)
	}
	if req.CostUSD != nil && *req.CostUSD < 0 {
		return nil, "", "", 0, fmt.Errorf("invalid cost %v: a cost is not negative", *req.CostUSD)
	}
	if req.Duration < 0 {
		return nil, "", "", 0, fmt.Errorf("invalid duration %v: a duration is not negative", req.Duration)
	}

	owner, repo, number, err := pr.ParsePRRef(req.PRRef)
	if err != nil {
		return nil, "", "", 0, err
	}

	pullReq, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, "", "", 0, fmt.Errorf("fetching PR: %w", err)
	}

	marker := &pr.RescueMarker{
		Tool:         req.Tool,
		Outcome:      outcome,
		Reason:       req.Reason,
		HeadSHA:      pullReq.GetHead().GetSHA(),
		At:           time.Now().UTC().Truncate(time.Second),
		Fingerprint:  process.FingerprintPR(ctx, client, owner, repo, pullReq),
		CostUSD:      req.CostUSD,
		DurationS:    int(req.Duration.Round(time.Second).Seconds()),
		DispatchedBy: req.DispatchedBy,
		RunID:        req.RunID,
	}

	if req.DryRun {
		return marker, owner, repo, number, nil
	}

	body := marker.CommentBody()
	_, _, err = client.Issues.CreateComment(ctx, owner, repo, number, github.IssueCommentRequest{Body: body})
	if err != nil {
		return nil, "", "", 0, fmt.Errorf("posting marker comment: %w", err)
	}

	return marker, owner, repo, number, nil
}
