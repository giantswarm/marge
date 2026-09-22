package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
)

// The catalogue is loaded at runtime, so `go test ./...` is what stands
// between a broken rule and every repository the sweep touches. These tests
// are the gate `marge rules validate` and `marge rules test` expose on the
// command line.

const catalogueDir = "rules"

func loadCatalogue(t *testing.T) *rules.Catalogue {
	t.Helper()
	catalogue, err := rules.Loader{LocalPath: catalogueDir}.Load(t.Context(), remedy.Default())
	require.NoError(t, err)
	return catalogue
}

// Every document of the catalogue validates. A document that does not would
// be skipped at runtime, so the rule it carries would silently not exist.
func TestCatalogueValidates(t *testing.T) {
	catalogue := loadCatalogue(t)

	for _, skipped := range catalogue.Skipped {
		t.Errorf("%s: %s", skipped.Path, skipped.Reason)
	}
	require.NotEmpty(t, catalogue.Rules, "the catalogue is empty")
}

// A rule without a scenario cannot land. Each one needs a case that shows it
// fires on its real signal and a case that shows it leaves a neighbouring
// failure alone.
func TestEveryRuleHasScenarios(t *testing.T) {
	catalogue := loadCatalogue(t)
	scenarios, err := rules.LoadScenarios(filepath.Join(catalogueDir, rules.ScenarioDir))
	require.NoError(t, err)

	coverage := rules.CheckCoverage(catalogue, scenarios)

	require.Empty(t, coverage.NoPositive, "rules with no scenario that matches them")
	require.Empty(t, coverage.NoNegative, "rules with no scenario that refuses them")
	require.Empty(t, coverage.Orphans, "scenario directories naming no rule")
}

// Every scenario replays to the outcome it states.
func TestScenarios(t *testing.T) {
	catalogue := loadCatalogue(t)
	scenarios, err := rules.LoadScenarios(filepath.Join(catalogueDir, rules.ScenarioDir))
	require.NoError(t, err)
	require.NotEmpty(t, scenarios)

	for _, scenario := range scenarios {
		t.Run(scenario.Rule+"/"+scenario.Name, func(t *testing.T) {
			if problem := scenario.Run(catalogue); problem != "" {
				t.Errorf("%s: %s", scenario.Path, problem)
			}
		})
	}
}

// loginFor is the trusted bot whose PRs a scenario of that kind describes.
// A scenario that names no kind is a Renovate PR, the commonest.
func loginFor(kind string) string {
	for _, login := range pr.TrustedLogins() {
		if string(pr.KindOf(login)) == kind {
			return login
		}
	}
	return "renovate[bot]"
}

// requestFor builds the most favourable action request a scenario allows: a
// trusted bot's open PR, no failing security check, every required context
// green, a head that has finished reporting, and no earlier attempt. A guard
// that still refuses here refuses for ever.
func requestFor(rule *rules.Rule, scenario *rules.Scenario) *remedy.Request {
	now := time.Now()
	req := &remedy.Request{
		Info: pr.PRInfo{Owner: "giantswarm", Repo: "marge", Number: 1},
		Pull: &github.PullRequest{
			User:  &github.User{Login: new(loginFor(scenario.Subject.Kind))},
			Base:  &github.PullRequestBranch{Ref: new("main")},
			State: new("open"),
		},
		Kind:            pr.Kind(scenario.Subject.Kind),
		Failing:         scenario.Subject.Failing,
		Required:        remedy.Required{Green: []string{"go-build"}},
		Now:             now,
		Reported:        len(scenario.Subject.Failing) + 1,
		ChecksSettledAt: now.Add(-24 * time.Hour),
		MissingContexts: scenario.Subject.MissingContexts,
		LogMatched:      rule.Match.Log != nil,
		Check:           scenario.Expect.Check,
		Commands:        scenario.Expect.Commands,
	}
	req.Required.Missing = scenario.Subject.MissingContexts
	return req
}

// No rule names an action whose guards refuse it on every PR. A scenario
// asserts which rule matched and never calls Apply, so without this a rule
// that matches and is then refused for ever passes every other test here.
func TestNoRuleIsRefusedForEver(t *testing.T) {
	catalogue := loadCatalogue(t)
	scenarios, err := rules.LoadScenarios(filepath.Join(catalogueDir, rules.ScenarioDir))
	require.NoError(t, err)
	registry := remedy.Default()

	positives := make(map[string]*rules.Scenario, len(scenarios))
	for _, scenario := range scenarios {
		if scenario.Expect.Rule == scenario.Rule {
			positives[scenario.Rule] = scenario
		}
	}

	for _, rule := range catalogue.Rules {
		t.Run(rule.Name, func(t *testing.T) {
			scenario := positives[rule.Name]
			require.NotNil(t, scenario, "no scenario matches this rule")

			action, known := registry.Lookup(rule.Action.Name)
			require.True(t, known)
			if held := registry.HeldReason(rule.Action.Name); held != "" {
				t.Skipf("%s is held: %s", rule.Action.Name, held)
			}

			request := requestFor(rule, scenario)
			for _, guard := range slices.Concat(action.Guards(), rule.Guards()) {
				require.Empty(t, guard.Refuse(request),
					"the rule matches and the action then refuses it, on every PR")
			}
		})
	}
}
