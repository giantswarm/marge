package remedy

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
var NotAlignFiles = Guard{"not-align-files", func(req *Request) string {
	if req.Kind == pr.KindAlignFiles {
		return "an Align files PR is regenerated, not closed"
	}
	return ""
}}

// closePR closes a PR nobody has to fix. The evidence the engine writes
// after it says why, so the action posts no comment of its own.
type closePR struct{}

func (closePR) Name() Name { return Close }

// A close is the one action nothing undoes, so it asks for the failing
// step's log like every other action that writes. A close decided from PR
// metadata alone is a Go change to this guard set, reviewed as code.
func (closePR) Guards() []Guard {
	return []Guard{TrustedAuthor, NoSecurityFailure, NotAlignFiles, LogMatched, OncePerChange(Close)}
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

func (markWait) Name() Name { return MarkWait }

// The classifier keeps a failing security check out of every remediable
// state by the check's name, which a security scan behind an ordinary job
// name escapes. The refusal is stated here rather than borrowed from it.
func (markWait) Guards() []Guard { return []Guard{TrustedAuthor, NoSecurityFailure} }

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

// cveWorkflow is the generated workflow every Go repository carries. It
// calls the shared fix-vulnerabilities workflow, which runs nancy-fixer and
// opens the remediation PR under the Herald App.
const cveWorkflow = "zz_generated.fix_vulnerabilities.yaml"

// dispatchCVEWorkflow triggers that workflow on the PR's base branch. The
// remedy for a finding the base head carries is a change to the base, and
// nancy-fixer performs it: the bump, the replace pin and the time-boxed
// .nancy-ignore entry belong to nancy-fixer, never to this engine.
type dispatchCVEWorkflow struct{}

func (dispatchCVEWorkflow) Name() Name { return DispatchCVEWorkflow }

// NoSecurityFailure is absent by design. It refuses every action on a PR
// whose failing check name matches the security list, which is the only
// state this action ever runs in, so carrying it would refuse for ever.
// NoGeneratedEdit is carried although the action writes no file: the set an
// action enforces is what a report prints.
func (dispatchCVEWorkflow) Guards() []Guard {
	return []Guard{TrustedAuthor, LogMatched, NoGeneratedEdit, OncePerChange(DispatchCVEWorkflow)}
}

func (dispatchCVEWorkflow) Apply(ctx context.Context, req *Request) (Outcome, error) {
	base := req.Pull.GetBase().GetRef()
	if base == "" {
		return Outcome{Refused: "the pull request names no base branch"}, nil
	}
	_, _, err := req.Deps.GitHub.Actions.CreateWorkflowDispatchEventByFileName(ctx,
		req.Info.Owner, req.Info.Repo, cveWorkflow,
		github.CreateWorkflowDispatchEventRequest{
			Ref:    base,
			Inputs: map[string]any{"branch": base},
		})
	if err != nil {
		return Outcome{}, fmt.Errorf("dispatch-cve-workflow: %w", err)
	}
	return Outcome{
		Applied:            true,
		KeepClassification: true,
		Detail:             "Fix Go vulnerabilities dispatched on " + base,
	}, nil
}

// fixProtectionContext drops the required status check contexts that no job
// posts any more, which a migration leaves behind and which never clear on
// their own. It only ever removes a context the head did not report, never
// adds one and never touches strict or enforce_admins.
type fixProtectionContext struct{}

func (fixProtectionContext) Name() Name { return FixProtectionContext }

func (fixProtectionContext) Guards() []Guard {
	return []Guard{TrustedAuthor, NoSecurityFailure, ChecksSettled, OncePerChange(FixProtectionContext)}
}

func (fixProtectionContext) Apply(ctx context.Context, req *Request) (Outcome, error) {
	if len(req.Required.Missing) == 0 {
		return Outcome{Refused: "every required context reported"}, nil
	}
	// The rule names the contexts it diagnosed. A context missing for
	// another reason stays required.
	stale := req.MissingContexts
	if len(stale) == 0 {
		return Outcome{Refused: "the rule named no missing context to drop"}, nil
	}
	if extra := outside(stale, req.Required.Missing); len(extra) > 0 {
		return Outcome{Refused: "the rule named a context the head reported: " + strings.Join(extra, ", ")}, nil
	}
	base := req.Pull.GetBase().GetRef()
	current, _, err := req.Deps.GitHub.Repositories.GetRequiredStatusChecks(ctx, req.Info.Owner, req.Info.Repo, base)
	if err != nil {
		return Outcome{}, fmt.Errorf("fix-protection-context: reading protection: %w", err)
	}

	kept := make([]string, 0, len(currentContexts(current)))
	var dropped []string
	for _, name := range currentContexts(current) {
		if slices.Contains(stale, name) {
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

// outside names the entries of have that want does not carry.
func outside(have, want []string) []string {
	var out []string
	for _, name := range have {
		if !slices.Contains(want, name) {
			out = append(out, name)
		}
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

// commentCommand comments the command a gate check named, so the suite the
// gate waits for starts. The command is the one the check reported: the
// rule captures it and the guard fences it, and this action composes none
// of its own.
type commentCommand struct{}

func (commentCommand) Name() Name { return CommentCommand }

// The comment writes no commit and dismisses no approval, so it asks for no
// log excerpt. It does ask that the gate be the only thing left, because
// the pipeline it starts costs a cluster.
func (commentCommand) Guards() []Guard {
	return []Guard{TrustedAuthor, NoSecurityFailure, KnownCommand, OnlyGateWaiting, OncePerChange(CommentCommand)}
}

func (commentCommand) Apply(ctx context.Context, req *Request) (Outcome, error) {
	body := strings.Join(req.Commands, "\n")
	_, _, err := req.Deps.GitHub.Issues.CreateComment(ctx, req.Info.Owner, req.Info.Repo, req.Info.Number,
		github.IssueCommentRequest{Body: body})
	if err != nil {
		return Outcome{}, fmt.Errorf("comment command: %w", err)
	}
	return Outcome{
		Applied:            true,
		KeepClassification: true,
		Detail:             "commented " + strings.Join(req.Commands, ", "),
	}, nil
}
