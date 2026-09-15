package rules

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
)

// testAction stands in for a registered action. Parsing resolves the action
// name against the registry and never calls the action.
type testAction struct{ name remedy.Name }

func (a testAction) Name() remedy.Name      { return a.name }
func (a testAction) Guards() []remedy.Guard { return nil }

func (a testAction) Apply(context.Context, *remedy.Request) (remedy.Outcome, error) {
	return remedy.Outcome{Applied: true}, nil
}

func testRegistry() *remedy.Registry {
	return remedy.NewRegistry(
		testAction{remedy.UpdateBranch},
		testAction{remedy.CircleCIRetry},
		testAction{remedy.Close},
	)
}

const validRule = `
name: circleci-auto-cancel
summary: A build CircleCI itself cancelled is not a verdict on the code.
source: runbook row 74 (L192)
match:
  states: [failed]
  kinds: [renovate, dependabot]
  check:
    name: "ci/circleci: *"
  log:
    source: circleci
    pattern: '"status"\s*:\s*"canceled"'
  pr:
    baseHead: green
action:
  name: circleci-retry
refuse:
  - once-per-change
  - log-matched
evidence:
  reason: "retried the auto-cancelled build"
`

func TestParseValidRule(t *testing.T) {
	rule, err := Parse("circleci-auto-cancel.yaml", []byte(validRule), testRegistry())
	require.NoError(t, err)

	require.Equal(t, "circleci-auto-cancel", rule.Name)
	require.Equal(t, remedy.CircleCIRetry, rule.Action.Name)
	require.Equal(t, map[pr.StatusState]bool{pr.StatusFailed: true}, rule.States())
	require.Equal(t, map[pr.Kind]bool{pr.KindRenovate: true, pr.KindDependabot: true}, rule.Kinds())
	require.NotNil(t, rule.LogPattern())
	require.True(t, rule.LogPattern().MatchString(`"status": "canceled"`))
	require.Len(t, rule.Guards(), 2)
	require.Equal(t, defaultLogExcerpt, rule.LogBytes())
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		doc     string
		wantErr string
	}{
		{
			name: "an unknown key",
			doc: validRule + `
allowUnguarded: true
`,
			wantErr: "field allowUnguarded not found",
		},
		{
			name:    "a name that does not match the file",
			file:    "other-name.yaml",
			doc:     validRule,
			wantErr: `must match the file name "other-name"`,
		},
		{
			name: "an unknown action",
			doc: `
name: bad-action
summary: s
source: runbook row 1
match:
  check:
    name: "*"
action:
  name: merge-everything
evidence:
  reason: y
`,
			wantErr: `unknown action "merge-everything"`,
		},
		{
			name: "no signal beyond states",
			doc: `
name: too-broad
summary: s
source: runbook row 1
match:
  states: [failed]
action:
  name: close
evidence:
  reason: y
`,
			wantErr: "match needs a check, log or pr signal",
		},
		{
			name: "an unknown state",
			doc: `
name: bad-state
summary: s
source: runbook row 1
match:
  states: [merged]
  check:
    name: "*"
action:
  name: close
evidence:
  reason: y
`,
			wantErr: `unknown state "merged"`,
		},
		{
			name: "an unknown kind",
			doc: `
name: bad-kind
summary: s
source: runbook row 1
match:
  kinds: [quentin]
  check:
    name: "*"
action:
  name: close
evidence:
  reason: y
`,
			wantErr: `unknown kind "quentin"`,
		},
		{
			name: "an unknown refusal",
			doc: `
name: bad-refusal
summary: s
source: runbook row 1
match:
  check:
    name: "*"
action:
  name: close
refuse: [skip-guards]
evidence:
  reason: y
`,
			wantErr: `unknown refusal "skip-guards"`,
		},
		{
			name: "a log pattern that does not compile",
			doc: `
name: bad-pattern
summary: s
source: runbook row 1
match:
  log:
    source: actions
    pattern: "("
action:
  name: close
evidence:
  reason: y
`,
			wantErr: "match.log.pattern",
		},
		{
			name: "an unknown log source",
			doc: `
name: bad-source
summary: s
source: runbook row 1
match:
  log:
    source: jenkins
    pattern: "x"
action:
  name: close
evidence:
  reason: y
`,
			wantErr: `unknown log source "jenkins"`,
		},
		{
			name: "a log excerpt beyond the bound",
			doc: `
name: too-much-log
summary: s
source: runbook row 1
match:
  log:
    source: actions
    pattern: "x"
    maxBytes: 99999999
action:
  name: close
evidence:
  reason: y
`,
			wantErr: "out of range",
		},
		{
			name: "a missing source citation",
			doc: `
name: no-source
summary: s
match:
  check:
    name: "*"
action:
  name: close
evidence:
  reason: y
`,
			wantErr: "source is required",
		},
		{
			name: "missing evidence",
			doc: `
name: no-evidence
summary: s
source: runbook row 1
match:
  check:
    name: "*"
action:
  name: close
`,
			wantErr: "evidence.reason is required",
		},
		{
			name:    "two documents in one file",
			doc:     validRule + "\n---\n" + validRule,
			wantErr: "one rule per file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := tt.file
			if file == "" {
				file = nameOf(tt.doc) + ".yaml"
			}
			_, err := Parse(file, []byte(tt.doc), testRegistry())
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// nameOf reads the name key of a test document so each case is filed under
// its own name without repeating it.
func nameOf(doc string) string {
	var probe struct {
		Name string `yaml:"name"`
	}
	_ = yaml.Unmarshal([]byte(doc), &probe)
	return probe.Name
}

func TestParseDuplicateRefusal(t *testing.T) {
	doc := `
name: repeated-refusal
summary: s
source: runbook row 1
match:
  check:
    name: "*"
action:
  name: close
refuse: [log-matched, log-matched]
evidence:
  reason: y
`
	_, err := Parse("repeated-refusal.yaml", []byte(doc), testRegistry())
	require.ErrorContains(t, err, `refusal "log-matched" listed twice`)
}

func TestParseEmptyStatesMatchEveryRemediableState(t *testing.T) {
	doc := `
name: any-state
summary: s
source: runbook row 1
match:
  check:
    name: "go-build"
action:
  name: update-branch
evidence:
  reason: y
`
	rule, err := Parse("any-state.yaml", []byte(doc), testRegistry())
	require.NoError(t, err)
	require.Empty(t, rule.States())
	require.Empty(t, rule.Kinds())
}
