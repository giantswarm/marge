package cmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/rules"
)

// A Node or Vite job colours its output and the Actions runner stamps every
// line, so a recorded excerpt carries escapes YAML forbids as content. The
// draft must still read back, or the first thing a person does with it is
// repair the fixture.
func TestWriteDraftReadsBack(t *testing.T) {
	dir := t.TempDir()
	rulesFlags.path = dir
	t.Cleanup(func() { rulesFlags.path = "rules" })

	group := &SweepUnhandled{
		Signature: "d6c0555bf440",
		Checks:    []string{"build", "ci/circleci: node-build"},
		Count:     1,
		PRs:       []string{"giantswarm/backstage#2250"},
		Excerpt: "2026-09-15T10:11:02.6059740Z \x1b[90m│\x1b[39m " +
			"\x1b[38;2;241;97;97mTypeError: Cannot read properties of undefined\x1b[39m\r\n" +
			"2026-09-15T10:11:02.6060980Z   at resolveTypescriptProject\n",
	}

	files, err := writeDraft("node-build-d6c055", group)
	require.NoError(t, err)
	require.Len(t, files, 3)

	scenarios, err := rules.LoadScenarios(filepath.Join(dir, rules.ScenarioDir))
	require.NoError(t, err, "the drafted scenarios do not parse")
	require.Len(t, scenarios, 2)

	var recorded string
	for _, s := range scenarios {
		if s.Expect.Rule == "" {
			continue
		}
		for _, excerpt := range s.Subject.Logs {
			recorded = excerpt
		}
	}
	require.Contains(t, recorded, "TypeError: Cannot read properties of undefined")
	require.NotContains(t, recorded, "\x1b")
	require.NotContains(t, recorded, "2026-09-15T10:11")
}
