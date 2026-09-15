package remedy

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// alignConfigRepo holds the Align files workflow and the team files.
const (
	alignConfigOwner = "giantswarm"
	alignConfigRepo  = "github"
	alignWorkflow    = "align-files.yaml"
	alignWorkflowRef = "main"
)

// NotAlignFiles refuses an action on an Align files PR. The alignment branch
// is regenerated every cycle, so closing such a PR only makes the bot open
// it again.
func NotAlignFiles(req *Request) string {
	if req.Kind == pr.KindAlignFiles {
		return "an Align files PR is regenerated, not closed"
	}
	return ""
}

// closePR closes a PR nobody has to fix. The evidence the engine writes
// after it says why, so the action posts no comment of its own.
type closePR struct{}

func (closePR) Name() Name { return Close }

// A close is decided from PR metadata -- a downgrade, a mangled pin, a
// sibling that supersedes it -- so it asks for no log excerpt. A rule whose
// own signal is a check name must add the log-matched refusal; validation
// enforces that.
func (closePR) Guards() []Guard {
	return []Guard{TrustedAuthor, NoSecurityFailure, NotAlignFiles, OncePerChange(Close)}
}

func (closePR) Apply(ctx context.Context, req *Request) (Outcome, error) {
	if req.Pull.GetState() == "closed" {
		return Outcome{Refused: "the PR is already closed"}, nil
	}
	_, _, err := req.Deps.GitHub.PullRequests.Edit(ctx, req.Info.Owner, req.Info.Repo, req.Info.Number,
		&github.PullRequest{State: new("closed")})
	if err != nil {
		return Outcome{}, fmt.Errorf("close: %w", err)
	}
	return Outcome{Applied: true, Detail: "closed"}, nil
}

// markWait records that a PR waits on something outside the repository and
// leaves it open under the classification the sweep gave it. Without the
// marker the next sweep re-reads the same failure and burns the same CI.
type markWait struct{}

func (markWait) Name() Name      { return MarkWait }
func (markWait) Guards() []Guard { return []Guard{TrustedAuthor} }

func (markWait) Apply(context.Context, *Request) (Outcome, error) {
	return Outcome{Applied: true, KeepClassification: true, Detail: "waiting on an external change"}, nil
}

// dispatchAlignWorkflow triggers the Align files workflow for one
// repository, which regenerates its alignment branch. It is the remedy for
// a template fix that has landed in giantswarm/github, never for a defect
// in the alignment branch itself.
type dispatchAlignWorkflow struct{}

func (dispatchAlignWorkflow) Name() Name { return DispatchAlignWorkflow }
func (dispatchAlignWorkflow) Guards() []Guard {
	return []Guard{TrustedAuthor, OncePerChange(DispatchAlignWorkflow)}
}

func (dispatchAlignWorkflow) Apply(ctx context.Context, req *Request) (Outcome, error) {
	_, _, err := req.Deps.GitHub.Actions.CreateWorkflowDispatchEventByFileName(ctx,
		alignConfigOwner, alignConfigRepo, alignWorkflow,
		github.CreateWorkflowDispatchEventRequest{
			Ref:    alignWorkflowRef,
			Inputs: map[string]any{"repository": req.Info.Repo},
		})
	if err != nil {
		return Outcome{}, fmt.Errorf("dispatch-align-workflow: %w", err)
	}
	return Outcome{
		Applied:            true,
		KeepClassification: true,
		Detail:             "Align files dispatched for " + req.Info.Repo,
	}, nil
}

// fixProtectionContext drops the required status check contexts that no job
// posts any more, which a migration leaves behind and which never clear on
// their own. It only ever removes a context the head did not report, never
// adds one and never touches strict or enforce_admins.
type fixProtectionContext struct{}

func (fixProtectionContext) Name() Name { return FixProtectionContext }

func (fixProtectionContext) Guards() []Guard {
	return []Guard{TrustedAuthor, NoSecurityFailure, OncePerChange(FixProtectionContext)}
}

func (fixProtectionContext) Apply(ctx context.Context, req *Request) (Outcome, error) {
	if len(req.Required.Missing) == 0 {
		return Outcome{Refused: "every required context reported"}, nil
	}
	base := req.Pull.GetBase().GetRef()
	current, _, err := req.Deps.GitHub.Repositories.GetRequiredStatusChecks(ctx, req.Info.Owner, req.Info.Repo, base)
	if err != nil {
		return Outcome{}, fmt.Errorf("fix-protection-context: reading protection: %w", err)
	}

	kept := make([]string, 0, len(currentContexts(current)))
	var dropped []string
	for _, name := range currentContexts(current) {
		if slices.Contains(req.Required.Missing, name) {
			dropped = append(dropped, name)
			continue
		}
		kept = append(kept, name)
	}
	switch {
	case len(dropped) == 0:
		return Outcome{Refused: "no stale required context on " + base}, nil
	case len(kept) == 0:
		// Every context would go. A branch with no required check is a
		// weaker branch than the one the migration left behind, so this is
		// a person's decision.
		return Outcome{Refused: "dropping " + fmt.Sprint(len(dropped)) + " contexts would leave " + base + " with none"}, nil
	}

	updated, _, err := req.Deps.GitHub.Repositories.UpdateRequiredStatusChecks(ctx, req.Info.Owner, req.Info.Repo, base,
		&github.RequiredStatusChecksRequest{Contexts: kept})
	if err != nil {
		return Outcome{}, fmt.Errorf("fix-protection-context: %w", err)
	}
	if left := intersect(currentContexts(updated), dropped); len(left) > 0 {
		return Outcome{StopRepository: true}, fmt.Errorf("fix-protection-context: %s still required on %s after the write", left[0], base)
	}
	return Outcome{
		Applied: true,
		Detail:  fmt.Sprintf("dropped stale required contexts on %s: %v", base, dropped),
	}, nil
}

// currentContexts reads a protection's context names from whichever of the
// two fields GitHub filled.
func currentContexts(checks *github.RequiredStatusChecks) []string {
	var out []string
	for _, c := range checks.GetChecks() {
		if c.Context != "" {
			out = append(out, c.Context)
		}
	}
	if len(out) == 0 {
		out = append(out, checks.GetContexts()...)
	}
	return out
}

func intersect(have, want []string) []string {
	var out []string
	for _, name := range have {
		if slices.Contains(want, name) {
			out = append(out, name)
		}
	}
	return out
}
