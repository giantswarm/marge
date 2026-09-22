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
    name: "go-*"
  pr:
    baseHead: absent
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
    name: "go-*"
  pr:
    baseHead: absent
action:
  name: update-branch
evidence:
  reason: y
`
	cat := catalogue(t, second, first)

	hit := cat.Match(&Subject{
		State:   pr.StatusFailed,
		Title:   "chore(deps): bump",
		Failing: []string{"go-build"},
	})

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
    baseHead: absent
action:
  name: close
evidence:
  reason: y
`
	cat := catalogue(t, doc)

	require.Nil(t, cat.Match(&Subject{State: pr.StatusFailed, Failing: []string{"go-build"}, Title: "fix: something"}))
	require.NotNil(t, cat.Match(&Subject{
		State:   pr.StatusFailed,
		Failing: []string{"go-build"},
		Title:   "chore(deps): update module sigs.k8s.io/controller-runtime to v0.23.3",
	}))
}

func TestMatchNilCatalogue(t *testing.T) {
	var missing *Catalogue
	require.Nil(t, missing.Match(&Subject{State: pr.StatusFailed}))
}

// A base-head condition speaks for every check the rule selected. One
// transient failure the base fixed does not carry a real failure beside it.
func TestMatchBaseHeadHoldsForEveryCandidate(t *testing.T) {
	cat := catalogue(t, behindRule)

	subject := &Subject{
		State:   pr.StatusStale,
		Failing: []string{"go-build", "go-test"},
		BaseState: map[string]CheckState{
			"go-build": CheckGreen,
			"go-test":  CheckRed,
		},
	}
	require.Nil(t, cat.Match(subject), "one check green on the base head is not every check")

	subject.BaseState["go-test"] = CheckGreen
	require.NotNil(t, cat.Match(subject))
}

// A PR waiting on a context nobody reported carries no failing check, so a
// protection signal is the only one that reaches it.
func TestMatchMissingContexts(t *testing.T) {
	cat := catalogue(t, `
name: context-drift
summary: A required context no job posts any more.
source: runbook rows 21 and 44
match:
  states: [waiting-checks]
  protection:
    missingContexts: ["ci/circleci: *"]
action:
  name: update-branch
evidence:
  reason: refreshed
`)

	require.Nil(t, cat.Match(&Subject{State: pr.StatusWaitingChecks}))
	require.Nil(t, cat.Match(&Subject{
		State:           pr.StatusWaitingChecks,
		MissingContexts: []string{"build / unit"},
	}))

	hit := cat.Match(&Subject{
		State:           pr.StatusWaitingChecks,
		MissingContexts: []string{"build / unit", "ci/circleci: go-build"},
	})
	require.NotNil(t, hit)
	require.Equal(t, "context-drift", hit.Rule.Name)
	require.Empty(t, hit.Check)
	require.Equal(t, []string{"ci/circleci: go-build"}, hit.MissingContexts,
		"the action rewrites only the contexts the rule named")
}

// anySourceRule reads the failing check's log whichever provider ran it.
const anySourceRule = `
name: module-proxy-dropped
summary: The module proxy dropped the connection while a module was fetched.
source: runbook row 91
match:
  states: [failed]
  log:
    pattern: 'proxy\.golang\.org.*stream error'
action:
  name: rerun-failed
evidence:
  reason: retried
`

// logsBySource answers for the provider that holds a log for the check, and
// reports nothing for every other, the way the sweep answers for a check
// only one provider ran.
func logsBySource(bodies map[LogSource]string) func(LogSource, string, int) (string, bool) {
	return func(source LogSource, _ string, _ int) (string, bool) {
		body, ok := bodies[source]
		return body, ok
	}
}

const proxyStreamError = `read "https://proxy.golang.org/cached-only/x/@v/v1.0.0.zip": stream error; stream ID 1415; INTERNAL_ERROR`

// A rule that names no source matches the failure on the provider that ran
// the check, and the provider that did not run it never answers.
func TestMatchWithoutASourceReadsCircleCI(t *testing.T) {
	cat := catalogue(t, anySourceRule)

	hit := cat.Match(&Subject{
		State:   pr.StatusFailed,
		Kind:    pr.KindRenovate,
		Failing: []string{"ci/circleci: go-build"},
		Log:     logsBySource(map[LogSource]string{LogCircleCI: proxyStreamError}),
	})

	require.NotNil(t, hit)
	require.Equal(t, "module-proxy-dropped", hit.Rule.Name)
	require.Equal(t, "ci/circleci: go-build", hit.Check)
	require.True(t, hit.LogMatched)
}

func TestMatchWithoutASourceReadsActions(t *testing.T) {
	cat := catalogue(t, anySourceRule)

	hit := cat.Match(&Subject{
		State:   pr.StatusFailed,
		Kind:    pr.KindRenovate,
		Failing: []string{"bootstrap"},
		Log:     logsBySource(map[LogSource]string{LogActions: proxyStreamError}),
	})

	require.NotNil(t, hit)
	require.Equal(t, "module-proxy-dropped", hit.Rule.Name)
	require.Equal(t, "bootstrap", hit.Check)
	require.True(t, hit.LogMatched)
}

// A rule that names a source keeps reading that source alone, so the same
// text on the other provider is not its failure.
func TestMatchWithASourceStillRestricts(t *testing.T) {
	doc := `
name: actions-only-proxy-drop
summary: The module proxy dropped the connection on an Actions run.
source: runbook row 91
match:
  states: [failed]
  log:
    source: actions
    pattern: 'proxy\.golang\.org.*stream error'
action:
  name: rerun-failed
evidence:
  reason: retried
`
	cat := catalogue(t, doc)
	subject := &Subject{
		State:   pr.StatusFailed,
		Kind:    pr.KindRenovate,
		Failing: []string{"ci/circleci: go-build"},
		Log:     logsBySource(map[LogSource]string{LogCircleCI: proxyStreamError}),
	}

	require.Nil(t, cat.Match(subject), "the rule reads the Actions log only")

	subject.Log = logsBySource(map[LogSource]string{LogActions: proxyStreamError})
	hit := cat.Match(subject)
	require.NotNil(t, hit)
	require.Equal(t, "actions-only-proxy-drop", hit.Rule.Name)
}

// gateRule matches a gate that has not finished and captures every command
// its message names.
func gateRule(t *testing.T) *Rule {
	t.Helper()
	doc := "" +
		"name: gate-waits\n" +
		"summary: the gate names a suite nobody started\n" +
		"source: giantswarm/marge#159\n" +
		"match:\n" +
		"  states: [waiting-checks]\n" +
		"  check:\n" +
		"    name: \"Heimdall - PR Gatekeeper\"\n" +
		"    state: pending\n" +
		"    output:\n" +
		"      pattern: \"wasn't found - you can trigger it by commenting on the PR with `(?P<command>/run [^`]+)`\"\n" +
		"action:\n" +
		"  name: update-branch\n" +
		"evidence:\n" +
		"  reason: the suite the gate waits for was never started\n"

	rule, err := Parse("gate-waits.yaml", []byte(doc), testRegistry())
	require.NoError(t, err)
	return rule
}

func TestMatchPendingCheck(t *testing.T) {
	const missing = "⚠️ Check Run `App E2E Test Suites - capa` is required but wasn't found - you can trigger it by commenting on the PR with `/run app-test-suites-single PROVIDER=capa`"

	tests := []struct {
		name    string
		pending []PendingCheck
		want    []string
	}{
		{
			name:    "the gate names one suite",
			pending: []PendingCheck{{Name: "Heimdall - PR Gatekeeper", Output: missing}},
			want:    []string{"/run app-test-suites-single PROVIDER=capa"},
		},
		{
			name: "the gate names one suite per provider",
			pending: []PendingCheck{{
				Name: "Heimdall - PR Gatekeeper",
				Output: missing + "\n" +
					"⚠️ Check Run `App E2E Test Suites - capz` is required but wasn't found - you can trigger it by commenting on the PR with `/run app-test-suites-single PROVIDER=capz`",
			}},
			want: []string{
				"/run app-test-suites-single PROVIDER=capa",
				"/run app-test-suites-single PROVIDER=capz",
			},
		},
		{
			name: "the same suite named twice is commented once",
			pending: []PendingCheck{{
				Name:   "Heimdall - PR Gatekeeper",
				Output: missing + "\n" + missing,
			}},
			want: []string{"/run app-test-suites-single PROVIDER=capa"},
		},
		{
			name: "another pending check carrying the same words is not the gate",
			pending: []PendingCheck{{
				Name:   "semantic-pull-request / Validate PR title",
				Output: missing,
			}},
		},
		{
			name:    "the gate reports no message yet",
			pending: []PendingCheck{{Name: "Heimdall - PR Gatekeeper"}},
		},
		{
			name: "the gate is waiting on a suite that is running",
			pending: []PendingCheck{{
				Name:   "Heimdall - PR Gatekeeper",
				Output: "⚠️ Check Run `App E2E Test Suites - capa` is required but is still in progress",
			}},
		},
		{
			name: "nothing is pending",
		},
	}

	catalogue := &Catalogue{Rules: []*Rule{gateRule(t)}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hit := catalogue.Match(&Subject{
				State:   pr.StatusWaitingChecks,
				Kind:    pr.KindRenovate,
				Pending: tc.pending,
			})
			if tc.want == nil {
				require.Nil(t, hit)
				return
			}
			require.NotNil(t, hit)
			require.Equal(t, "Heimdall - PR Gatekeeper", hit.Check)
			require.Equal(t, tc.want, hit.Commands)
		})
	}
}

// A rule reading the pending checks never reads the failing ones. The two
// lists answer different questions, and a gate that finally fails is a
// failure like any other.
func TestMatchPendingIgnoresFailingChecks(t *testing.T) {
	catalogue := &Catalogue{Rules: []*Rule{gateRule(t)}}

	hit := catalogue.Match(&Subject{
		State:   pr.StatusWaitingChecks,
		Kind:    pr.KindRenovate,
		Failing: []string{"Heimdall - PR Gatekeeper"},
	})
	require.Nil(t, hit)
}
