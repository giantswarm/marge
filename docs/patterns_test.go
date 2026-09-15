package docs

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ruleRow matches one body row of the "Patterns the engine acts on" table in
// patterns.md, whose second cell names the rule file:
// "| A CircleCI build the platform cancelled | `circleci-auto-cancel` | ... |".
var ruleRow = regexp.MustCompile("(?m)^\\| [^|]+ \\| `([a-z0-9-]+)` \\| `([a-z-]+)` \\|")

// The table is the catalogue's index, and an index nobody checks is an index
// that drifts. A rule added without its row leaves patterns.md claiming to
// carry every pattern the engine acts on while it does not.
func TestPatternsIndexesEveryRule(t *testing.T) {
	page, err := os.ReadFile("patterns.md")
	require.NoError(t, err)

	indexed := make(map[string]string)
	for _, row := range ruleRow.FindAllStringSubmatch(string(page), -1) {
		indexed[row[1]] = row[2]
	}

	files, err := os.ReadDir("../rules")
	require.NoError(t, err)
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(f.Name(), ".yaml")
		action, listed := indexed[name]
		require.True(t, listed, "rule %s has no row in docs/patterns.md", name)

		rule, err := os.ReadFile("../rules/" + f.Name())
		require.NoError(t, err)
		require.Contains(t, string(rule), "name: "+action,
			"docs/patterns.md names action %q for rule %s", action, name)
		delete(indexed, name)
	}
	require.Empty(t, indexed, "docs/patterns.md indexes rules that do not exist")
}

// Every row of the table cites where it came from, which is what lets the
// runbook be retired: a row with no citation cannot be matched to one.
func TestPatternsRowsCiteTheirSource(t *testing.T) {
	page, err := os.ReadFile("patterns.md")
	require.NoError(t, err)

	for _, line := range strings.Split(string(page), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		last := strings.TrimSpace(cells[len(cells)-1])
		if last == "Runbook row" || last == "Where the fix goes" || last == "Why the sweep leaves it" ||
			last == "Where the fix belongs" || last == "Why it is not mechanical" ||
			last == "Why no rule expresses it" || last == "Action" {
			continue
		}
		require.Regexp(t, `\d`, last, "row cites no runbook row: %s", line)
	}
}
