package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

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
