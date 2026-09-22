// Package remedy holds the closed vocabulary of actions a rule may name and
// the guards each action enforces. A rule selects an action by name and may
// add refusal conditions; it has no way to remove one, so a rule can never
// do what its action forbids.
package remedy

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
)

// Name identifies one action of the vocabulary. Only a registered name is
// valid in a rule.
type Name string

const (
	// UpdateBranch merges the base branch into the PR head, the way the
	// "Update branch" button does.
	UpdateBranch Name = "update-branch"
	// RerunFailed reruns the failed jobs of the build behind the matched
	// check, on the provider that ran it.
	RerunFailed Name = "rerun-failed"
	// CircleCIRetry reruns a CircleCI workflow from its failed jobs, or
	// retries the single build when no workflow is known.
	CircleCIRetry Name = "circleci-retry"
	// Close closes a PR that nobody has to fix, with a comment.
	Close Name = "close"
	// FixProtectionContext rewrites the required status check contexts of a
	// base branch after a migration renamed the jobs that post them.
	FixProtectionContext Name = "fix-protection-context"
	// DispatchCVEWorkflow triggers a repository's generated Fix Go
	// vulnerabilities workflow on the pull request's base branch, which
	// runs nancy-fixer and opens the remediation PR under the Herald App.
	DispatchCVEWorkflow Name = "dispatch-cve-workflow"
	// MarkWait records that the PR waits on something outside the
	// repository and leaves it open.
	MarkWait Name = "mark-wait"
	// CommentCommand comments the command a gate check named, so the test
	// suite the gate waits for starts.
	CommentCommand Name = "comment-command"
)

// Deps are the clients an action calls. CircleCI is nil when no token is
// available; an action that needs it refuses rather than guesses.
type Deps struct {
	GitHub   *github.Client
	CircleCI *circleci.Client
	// Login is the authenticated account, used to recognise the sweep's own
	// review and its own markers.
	Login string
}

// Required partitions a base branch's required contexts by what the PR head
// reported for them. A context nobody reported is Missing, and missing is a
// wait.
type Required struct {
	Green   []string
	Pending []string
	Failed  []string
	Missing []string
}

// AllGreen reports whether every required context reported success.
func (r Required) AllGreen() bool {
	return len(r.Pending) == 0 && len(r.Failed) == 0 && len(r.Missing) == 0
}

// BaseStates partitions the PR's failing checks by what the base head
// reported for the same name. A check the base head never ran is Absent,
// and absent is not green.
type BaseStates struct {
	Green  []string
	Red    []string
	Absent []string
}

// Request is everything an action reads. The engine fills it from the
// classification; an action performs no classification of its own.
type Request struct {
	Info   pr.PRInfo
	Pull   *github.PullRequest
	Kind   pr.Kind
	Update pr.UpdateType
	DryRun bool

	// Now is the time the sweep read this PR. Guards read it rather than the
	// clock, so every one of them stays pure.
	Now time.Time

	// Failing names every red check on the head that produced a verdict.
	Failing []string
	// SecurityFailure names the failing check matching the security pattern
	// list, or "" when none does.
	SecurityFailure string
	Required        Required
	Base            BaseStates

	// Reported counts the contexts the head reported in any state.
	Reported int
	// ChecksPending reports whether a check on the head has not finished.
	ChecksPending bool
	// ChecksSettledAt is the newest completion time among the head's
	// checks. Zero means none of them carries one.
	ChecksSettledAt time.Time
	// MissingContexts are the required contexts a rule's protection signal
	// selected. An action that rewrites a protection touches these and no
	// other.
	MissingContexts []string

	// Check is the failing check the rule matched, and CheckURL is where its
	// build or job lives: a CircleCI build behind a commit status, an
	// Actions job behind a check run.
	Check    string
	CheckURL string

	// LogMatched reports whether the rule matched an excerpt of the failing
	// step's log rather than a check name alone.
	LogMatched bool
	// Commands are the strings a rule's output signal captured from a
	// check's own message. An action that comments one writes these
	// verbatim and composes none of its own.
	Commands []string
	// AppliedThisChange names the actions an existing marker records for the
	// change currently on the branch.
	AppliedThisChange map[Name]bool
	// Writes names the repository paths the action would write.
	Writes []string

	Deps Deps
}

// Outcome is what an action did. Exactly one of Applied and Refused is set.
type Outcome struct {
	Applied bool
	// Refused carries the guard's reason when a guard stopped the action.
	Refused string
	// Detail is the operator-facing summary of an applied action.
	Detail string
	// StopRepository ends the sweep for this repository, for instance when a
	// protection write left a required context the head cannot report.
	StopRepository bool
	// KeepClassification leaves the PR in the state the sweep classified it
	// in. An action that only records why a PR waits sets it.
	KeepClassification bool
}

// Action is one remedy of the vocabulary.
type Action interface {
	Name() Name
	// Guards are enforced on every call, before Apply, and are not
	// addressable from a rule document.
	Guards() []Guard
	Apply(context.Context, *Request) (Outcome, error)
}

// Registry is the set of actions this build implements.
type Registry struct {
	byName map[Name]Action
}

// NewRegistry indexes the actions by name. A duplicate name panics: the
// vocabulary is built once at start-up.
func NewRegistry(actions ...Action) *Registry {
	reg := &Registry{byName: make(map[Name]Action, len(actions))}
	for _, a := range actions {
		if _, dup := reg.byName[a.Name()]; dup {
			panic(fmt.Sprintf("remedy: action %q registered twice", a.Name()))
		}
		reg.byName[a.Name()] = a
	}
	return reg
}

// Lookup returns the action with that name.
func (r *Registry) Lookup(name Name) (Action, bool) {
	a, ok := r.byName[name]
	return a, ok
}

// GuardNames lists the guards an action enforces, in the order it runs them.
func (r *Registry) GuardNames(name Name) []string {
	action, ok := r.Lookup(name)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(action.Guards()))
	for _, guard := range action.Guards() {
		out = append(out, guard.Name)
	}
	return out
}

// Names lists every implemented action, sorted, for validation errors and
// help text.
func (r *Registry) Names() []Name {
	out := make([]Name, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Apply runs the action's own guards, then the rule's extra refusals, and
// calls the action only when every one of them passed. extra can add a
// refusal; there is no path by which it removes one.
func (r *Registry) Apply(ctx context.Context, name Name, req *Request, extra []Guard) (Outcome, error) {
	action, ok := r.Lookup(name)
	if !ok {
		return Outcome{}, fmt.Errorf("remedy: unknown action %q: known actions are %s", name, joinNames(r.Names()))
	}
	if reason := refuse(slices.Concat(action.Guards(), extra), req); reason != "" {
		return Outcome{Refused: reason}, nil
	}
	return action.Apply(ctx, req)
}

// refuse returns the first guard's reason, or "" when every one of them
// passed. An action that runs another action's Apply calls it with that
// action's guards, so no path reaches a write past them.
func refuse(guards []Guard, req *Request) string {
	for _, guard := range guards {
		if reason := guard.Refuse(req); reason != "" {
			return reason
		}
	}
	return ""
}

func joinNames(names []Name) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return strings.Join(out, ", ")
}
