package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
)

// recordingAction stands in for a remedy: it records the request it was
// given and applies without calling GitHub.
type recordingAction struct {
	name     remedy.Name
	guards   []remedy.Guard
	requests *[]*remedy.Request
}

func (a recordingAction) Name() remedy.Name      { return a.name }
func (a recordingAction) Guards() []remedy.Guard { return a.guards }

func (a recordingAction) Apply(_ context.Context, req *remedy.Request) (remedy.Outcome, error) {
	*a.requests = append(*a.requests, req)
	return remedy.Outcome{Applied: true, Detail: "applied"}, nil
}

// ruleFor writes a one-rule catalogue that matches every go check of the
// given state.
func ruleFor(t *testing.T, state string) *rules.Catalogue {
	t.Helper()
	dir := t.TempDir()
	doc := `name: catch-all
summary: Matches every go check, for the stage test.
source: test
match:
  states: [` + state + `]
  check:
    name: "go*"
  pr:
    titlePattern: "."
action:
  name: update-branch
evidence:
  reason: applied by the catch-all rule
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "catch-all.yaml"), []byte(doc), 0o600))

	cat, err := rules.Loader{LocalPath: dir}.Load(t.Context(), remedy.NewRegistry(
		recordingAction{name: remedy.UpdateBranch, requests: new([]*remedy.Request)},
	))
	require.NoError(t, err)
	require.Empty(t, cat.Skipped)
	return cat
}

func remedyProcessor(t *testing.T, state string, requests *[]*remedy.Request) *Processor {
	t.Helper()
	return &Processor{
		Login:    "marge",
		Actions:  ActionSet{ActionClassify: true, ActionRemedy: true},
		Rules:    ruleFor(t, state),
		Remedies: remedy.NewRegistry(recordingAction{name: remedy.UpdateBranch, requests: requests}),
	}
}

func remedyRun(state pr.StatusState) *prRun {
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "giantswarm", Repo: "marge", Number: 1}
	idx := status.Add(info)
	status.Update(idx, state, "")
	return &prRun{
		info:   info,
		status: status,
		idx:    idx,
		kind:   pr.KindRenovate,
		pull: &github.PullRequest{
			User:  &github.User{Login: new("renovate[bot]")},
			Title: new("chore(deps): update module"),
			Head:  &github.PullRequestBranch{SHA: new("headsha")},
			Base:  &github.PullRequestBranch{Ref: new("main")},
		},
		failing:        []string{"go-build"},
		filesLoaded:    true,
		commentsLoaded: true,
	}
}

// The rule stage runs for every remediable classification and for no
// other. TestProcessPRReachesTheRuleStage covers the wiring that gets a PR
// here.
func TestRuleStageRunsForEveryRemediableState(t *testing.T) {
	states := map[string]pr.StatusState{
		"failed":            pr.StatusFailed,
		"stale":             pr.StatusStale,
		"conflict":          pr.StatusConflict,
		"cancelled":         pr.StatusCancelled,
		"no-verdict":        pr.StatusNoVerdict,
		"blocked-ci":        pr.StatusBlockedCI,
		"obsolete":          pr.StatusObsolete,
		"waiting-checks":    pr.StatusWaitingChecks,
		"awaiting-approval": pr.StatusAwaitingApproval,
		"held":              pr.StatusHeld,
	}

	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			var requests []*remedy.Request
			p := remedyProcessor(t, name, &requests)
			run := remedyRun(state)

			p.applyRule(t.Context(), run)

			require.Len(t, requests, 1, "the rule stage did not run for %s", name)
			require.Equal(t, pr.StatusRemedied, run.status.StateAt(run.idx))
		})
	}
}

// Every state a rule may name is remediable, and no other state is. The
// catalogue schema and the engine agree on one list.
func TestRemediableStatesAreTheOnesRulesMayName(t *testing.T) {
	require.True(t, rules.Remediable(pr.StatusFailed))
	require.False(t, rules.Remediable(pr.StatusMerged))
	require.False(t, rules.Remediable(pr.StatusFailedSecurity))
	require.False(t, rules.Remediable(pr.StatusUntrustedAuthor))
	require.False(t, rules.Remediable(pr.StatusRemedied))
}

func TestRuleStageSkippedWithoutTheRemedyAction(t *testing.T) {
	var requests []*remedy.Request
	p := remedyProcessor(t, "failed", &requests)
	p.Actions = ActionSet{ActionClassify: true}

	p.applyRule(t.Context(), remedyRun(pr.StatusFailed))

	require.Empty(t, requests)
}

func TestRuleStageSkippedWithoutACatalogue(t *testing.T) {
	var requests []*remedy.Request
	p := remedyProcessor(t, "failed", &requests)
	p.Rules = nil

	p.applyRule(t.Context(), remedyRun(pr.StatusFailed))

	require.Empty(t, requests)
}

// A dry run reports the rule that would apply and writes nothing.
func TestRuleStageDryRun(t *testing.T) {
	var requests []*remedy.Request
	p := remedyProcessor(t, "failed", &requests)
	p.DryRun = true
	run := remedyRun(pr.StatusFailed)

	p.applyRule(t.Context(), run)

	require.Empty(t, requests)
	require.Equal(t, pr.StatusFailed, run.status.StateAt(run.idx))
	require.Contains(t, run.notes, "dry-run: rule catch-all would apply update-branch")
}

// A guard of the action refuses although the rule named no refusal: the
// outcome is a note, and the classification stands.
func TestRuleStageReportsAGuardRefusal(t *testing.T) {
	var requests []*remedy.Request
	p := remedyProcessor(t, "failed", &requests)
	p.Remedies = remedy.NewRegistry(recordingAction{
		name:     remedy.UpdateBranch,
		guards:   []remedy.Guard{remedy.NoSecurityFailure},
		requests: &requests,
	})
	run := remedyRun(pr.StatusFailed)
	run.failing = []string{"govulncheck"}

	p.applyRule(t.Context(), run)

	require.Empty(t, requests)
	require.Equal(t, pr.StatusFailed, run.status.StateAt(run.idx))
	require.Contains(t, run.notes[0], "rule catch-all refused: security check failed: govulncheck")
}
