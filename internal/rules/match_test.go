package rules

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

func catalogue(t *testing.T, docs ...string) *Catalogue {
	t.Helper()
	files := make(map[string]string, len(docs))
	for _, doc := range docs {
		files[nameOf(doc)+".yaml"] = doc
	}
	cat, err := Loader{LocalPath: writeCatalogue(t, files)}.Load(t.Context(), testRegistry())
	require.NoError(t, err)
	require.Empty(t, cat.Skipped)
	return cat
}

const cancelRule = `
name: circleci-auto-cancel
summary: A build CircleCI itself cancelled is not a verdict on the code.
source: runbook row 74
match:
  states: [failed]
  check:
    name: "ci/circleci: *"
  log:
    source: circleci
    pattern: 'canceled'
action:
  name: circleci-retry
evidence:
  reason: retried
`

const behindRule = `
name: behind-base
summary: A PR behind its base whose failure is green on the base head.
source: runbook row 68
match:
  states: [stale]
  pr:
    baseHead: green
action:
  name: update-branch
evidence:
  reason: refreshed
`

func fileList(paths ...string) func() []string {
	return func() []string { return paths }
}

func logReturning(body string) func(LogSource, string, int) (string, bool) {
	return func(LogSource, string, int) (string, bool) { return body, true }
}

func TestMatchLogSignal(t *testing.T) {
	cat := catalogue(t, cancelRule)

	hit := cat.Match(&Subject{
		State:   pr.StatusFailed,
		Kind:    pr.KindRenovate,
		Failing: []string{"ci/circleci: go-build"},
		Log:     logReturning(`{"status":"canceled"}`),
	})

	require.NotNil(t, hit)
	require.Equal(t, "circleci-auto-cancel", hit.Rule.Name)
	require.Equal(t, "ci/circleci: go-build", hit.Check)
	require.True(t, hit.LogMatched)
}

// An unreadable log leaves the failure as classified: a check name alone
// never stands in for the evidence the rule asked for.
func TestMatchRefusesWhenTheLogCannotBeRead(t *testing.T) {
	cat := catalogue(t, cancelRule)
	subject := &Subject{
		State:   pr.StatusFailed,
		Kind:    pr.KindRenovate,
		Failing: []string{"ci/circleci: go-build"},
	}

	require.Nil(t, cat.Match(subject), "no log fetcher")

	subject.Log = func(LogSource, string, int) (string, bool) { return "", false }
	require.Nil(t, cat.Match(subject), "the fetch failed")

	subject.Log = logReturning("everything is fine")
	require.Nil(t, cat.Match(subject), "the excerpt does not carry the signal")
}

func TestMatchCheckGlob(t *testing.T) {
	cat := catalogue(t, cancelRule)

	hit := cat.Match(&Subject{
		State:   pr.StatusFailed,
		Failing: []string{"go-build", "pre-commit"},
		Log:     logReturning("canceled"),
	})

	require.Nil(t, hit, "no failing check matches the glob")
}

func TestMatchState(t *testing.T) {
	cat := catalogue(t, cancelRule)

	hit := cat.Match(&Subject{
		State:   pr.StatusConflict,
		Failing: []string{"ci/circleci: go-build"},
		Log:     logReturning("canceled"),
	})

	require.Nil(t, hit)
}

// A check the base head never ran is absent, and absent is not green.
func TestMatchBaseHeadAbsentIsNotGreen(t *testing.T) {
	cat := catalogue(t, behindRule)

	subject := &Subject{
		State:     pr.StatusStale,
		Failing:   []string{"go-build"},
		BaseState: map[string]CheckState{},
	}
	require.Nil(t, cat.Match(subject))

	subject.BaseState["go-build"] = CheckRed
	require.Nil(t, cat.Match(subject))

	subject.BaseState["go-build"] = CheckGreen
	hit := cat.Match(subject)
	require.NotNil(t, hit)
	require.Equal(t, "behind-base", hit.Rule.Name)
	require.False(t, hit.LogMatched)
}

// The first rule in name order wins, so a catalogue decides the same way
// however it was read.
func TestMatchTakesTheFirstRuleByName(t *testing.T) {
	second := `
name: zz-later-rule
summary: s
source: runbook row 1
match:
  states: [failed]
  check:
    name: "*"
action:
  name: close
evidence:
  reason: y
`
	first := `
name: aa-earlier-rule
summary: s
source: runbook row 1
match:
  states: [failed]
  check:
    name: "*"
action:
  name: update-branch
evidence:
  reason: y
`
	cat := catalogue(t, second, first)

	hit := cat.Match(&Subject{State: pr.StatusFailed, Failing: []string{"go-build"}})

	require.NotNil(t, hit)
	require.Equal(t, "aa-earlier-rule", hit.Rule.Name)
}

func TestMatchFileGlobs(t *testing.T) {
	doc := `
name: vendored-chart
summary: s
source: runbook row 39
match:
  states: [failed]
  pr:
    files: ["helm/*/charts/**"]
action:
  name: close
evidence:
  reason: y
`
	cat := catalogue(t, doc)

	require.Nil(t, cat.Match(&Subject{State: pr.StatusFailed, Files: fileList("helm/marge/values.yaml")}))
	require.NotNil(t, cat.Match(&Subject{
		State: pr.StatusFailed,
		Files: fileList("helm/marge/charts/sub/Chart.yaml"),
	}))
	require.Nil(t, cat.Match(&Subject{State: pr.StatusFailed}), "a diff that cannot be read never matches a file signal")
}

func TestMatchTitlePattern(t *testing.T) {
	doc := `
name: downgrade
summary: s
source: runbook row 16
match:
  states: [failed]
  pr:
    titlePattern: '^chore\(deps\): update module '
action:
  name: close
evidence:
  reason: y
`
	cat := catalogue(t, doc)

	require.Nil(t, cat.Match(&Subject{State: pr.StatusFailed, Title: "fix: something"}))
	require.NotNil(t, cat.Match(&Subject{
		State: pr.StatusFailed,
		Title: "chore(deps): update module sigs.k8s.io/controller-runtime to v0.23.3",
	}))
}

func TestMatchNilCatalogue(t *testing.T) {
	var missing *Catalogue
	require.Nil(t, missing.Match(&Subject{State: pr.StatusFailed}))
}
