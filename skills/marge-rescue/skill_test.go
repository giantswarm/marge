package margerescue

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The hint table's rows cite the runbook rows they come from, in the last
// cell: "| A stale `go.sum` ... | `go mod tidy`, commit, push | 1 |".
var hintRow = regexp.MustCompile(`(?m)^\|[^|]+\|[^|]+\|\s*([0-9, and]+?)\s*\|$`)

// A rule's provenance: "source: runbook rows 7, 9, 15 and 31 (L125, L127)".
var ruleSource = regexp.MustCompile(`(?m)^source: runbook rows? ([0-9, and]+)`)

var number = regexp.MustCompile(`\d+`)

func rows(t *testing.T, matches [][]string) map[string]bool {
	t.Helper()
	found := make(map[string]bool)
	for _, match := range matches {
		for _, row := range number.FindAllString(match[1], -1) {
			found[row] = true
		}
	}
	return found
}

func skill(t *testing.T) string {
	t.Helper()
	page, err := os.ReadFile("SKILL.md")
	require.NoError(t, err)
	return string(page)
}

// A hint for a pattern the engine now fixes by itself is spent tokens on
// every run. The skill lives in this repository so that promoting a pattern
// to a rule and deleting its hint is one pull request; this test is what
// makes the deletion mandatory rather than remembered.
func TestHintsDoNotRepeatTheRules(t *testing.T) {
	hinted := rows(t, hintRow.FindAllStringSubmatch(skill(t), -1))
	require.NotEmpty(t, hinted, "the hint table parsed empty")

	files, err := os.ReadDir("../../rules")
	require.NoError(t, err)
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}
		rule, err := os.ReadFile("../../rules/" + file.Name())
		require.NoError(t, err)
		for row := range rows(t, ruleSource.FindAllStringSubmatch(string(rule), -1)) {
			require.False(t, hinted[row],
				"rule %s covers runbook row %s, which SKILL.md still hints", file.Name(), row)
		}
	}
}

// docs/patterns.md decides which patterns reach a person or an agent. The
// three sections below are the ones a rescue run acts on; the others are
// marked and left, and a hint for them would send the model to work the sweep
// refuses to do. Every row of those sections is a hint, and every hint is one
// of those rows.
func TestHintsMatchThePatternsPage(t *testing.T) {
	page, err := os.ReadFile("../../docs/patterns.md")
	require.NoError(t, err)

	const section = "### "
	forTheAgent := []string{
		"Waiting on a branch-writing remedy",
		"Fixed on the default branch first",
		"Needing a person or an agent",
	}

	// The CVE rows are on the page for completeness only. Herald's
	// nancy-fixer performs the bump and the time-boxed ignore, and marge
	// never re-implements that remedy, so a hint would send the model to do
	// another tool's work.
	herald := []string{"23", "51", "81", "98"}

	expected := make(map[string]bool)
	for _, block := range strings.Split(string(page), section)[1:] {
		heading, body, _ := strings.Cut(block, "\n")
		if !slices.Contains(forTheAgent, strings.TrimSpace(heading)) {
			continue
		}
		for row := range rows(t, hintRow.FindAllStringSubmatch(body, -1)) {
			if !slices.Contains(herald, row) {
				expected[row] = true
			}
		}
	}
	require.NotEmpty(t, expected, "no agent-facing section parsed out of docs/patterns.md")

	hinted := rows(t, hintRow.FindAllStringSubmatch(skill(t), -1))
	for row := range expected {
		require.True(t, hinted[row], "docs/patterns.md row %s reaches the agent with no hint in SKILL.md", row)
	}
	for row := range hinted {
		require.True(t, expected[row], "SKILL.md hints runbook row %s, which docs/patterns.md does not send to an agent", row)
	}
}
