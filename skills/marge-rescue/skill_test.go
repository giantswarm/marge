package margerescue

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A citation of runbook rows, as both pages and every rule write it:
// "1", "36, 46", "7, 9, 15 and 31".
const citation = `\d+(?:(?:,\s*|\s+and\s+)\d+)*`

// A rule's provenance: "source: runbook rows 7, 9, 15 and 31 (L125, L127)".
var ruleSource = regexp.MustCompile(`(?m)^source: runbook rows? (` + citation + `)`)

var citedRows = regexp.MustCompile(`^` + citation + `$`)

var number = regexp.MustCompile(`\d+`)

// row is one body row of a table whose last cell cites the runbook rows it
// comes from. A header or a separator cites nothing, so it is not a row.
type row struct {
	what   string
	remedy string
	cited  []string
}

func table(markdown string) []row {
	var rows []row
	for _, line := range strings.Split(markdown, "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 3 {
			continue
		}
		cited := strings.TrimSpace(cells[len(cells)-1])
		if !citedRows.MatchString(cited) {
			continue
		}
		rows = append(rows, row{
			what:   strings.TrimSpace(cells[0]),
			remedy: strings.TrimSpace(cells[1]),
			cited:  number.FindAllString(cited, -1),
		})
	}
	return rows
}

func cited(rows []row) map[string]bool {
	found := make(map[string]bool)
	for _, one := range rows {
		for _, number := range one.cited {
			found[number] = true
		}
	}
	return found
}

func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

// A hint for a pattern the engine now fixes by itself is spent tokens on
// every run. The skill lives in this repository so that promoting a pattern
// to a rule and deleting its hint is one pull request; this test is what
// makes the deletion mandatory rather than remembered.
func TestHintsDoNotRepeatTheRules(t *testing.T) {
	hinted := cited(table(read(t, "SKILL.md")))
	require.NotEmpty(t, hinted, "the hint table parsed empty")

	files, err := os.ReadDir("../../rules")
	require.NoError(t, err)
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}
		rule := read(t, "../../rules/"+file.Name())
		for _, source := range ruleSource.FindAllStringSubmatch(rule, -1) {
			for _, number := range number.FindAllString(source[1], -1) {
				require.False(t, hinted[number],
					"rule %s covers runbook row %s, which SKILL.md still hints", file.Name(), number)
			}
		}
	}
}

// docs/patterns.md decides which patterns reach a person or an agent. Every
// row of a section a rescue run acts on is a hint, and every hint is one of
// those rows. The other sections are marked and left, and a hint for them
// would send the model to work the sweep refuses to do.
func TestHintsMatchThePatternsPage(t *testing.T) {
	forTheAgent := []string{
		"Waiting on a branch-writing remedy",
		"Fixed on the default branch first",
		"Needing a person or an agent",
	}
	notForTheAgent := []string{
		"Held by a team decision",
		"Belonging upstream",
	}

	expected := make(map[string]bool)
	var seen []string
	var heading string
	for _, line := range strings.Split(read(t, "../../docs/patterns.md"), "\n") {
		if after, found := strings.CutPrefix(line, "### "); found {
			heading = strings.TrimSpace(after)
			seen = append(seen, heading)
			require.True(t,
				slices.Contains(forTheAgent, heading) || slices.Contains(notForTheAgent, heading),
				"docs/patterns.md section %q is in neither list; decide whether a rescue run acts on it", heading)
		} else if strings.HasPrefix(line, "## ") {
			heading = ""
		}
		if !slices.Contains(forTheAgent, heading) {
			continue
		}
		for _, one := range table(line) {
			// nancy-fixer performs the bump and the time-boxed ignore,
			// and marge never re-implements that remedy, so a hint would
			// send the model to do another tool's work.
			if strings.Contains(one.remedy, "nancy-fixer") {
				continue
			}
			for _, number := range one.cited {
				expected[number] = true
			}
		}
	}
	for _, section := range forTheAgent {
		require.Contains(t, seen, section, "docs/patterns.md has no section %q", section)
	}
	require.NotEmpty(t, expected, "no agent-facing section parsed out of docs/patterns.md")

	hinted := cited(table(read(t, "SKILL.md")))
	for number := range expected {
		require.True(t, hinted[number], "docs/patterns.md row %s reaches the agent with no hint in SKILL.md", number)
	}
	for number := range hinted {
		require.True(t, expected[number], "SKILL.md hints runbook row %s, which docs/patterns.md does not send to an agent", number)
	}
}
