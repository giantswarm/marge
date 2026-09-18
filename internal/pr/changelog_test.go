package pr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const keepAChangelog = `# Changelog

All notable changes to this project are documented in this file.

## [Unreleased]

### Added

- A thing.

### Changed

- Another thing.

## [1.2.0] - 2026-09-01

### Added

- The first thing.
`

func TestChangelogLine_defaultFormat(t *testing.T) {
	policy := ChangelogDefaults()

	both, err := ChangelogLine(policy, ChangelogFacts{
		Dependency: "lodash", From: "4.17.20", To: "4.17.21", PR: "giantswarm/backstage#2250",
	})
	require.NoError(t, err)
	require.Equal(t, "- Update lodash from 4.17.20 to 4.17.21 (giantswarm/backstage#2250)", both)

	target, err := ChangelogLine(policy, ChangelogFacts{
		Dependency: "giantswarm/pause", To: "v3.10.2", PR: "giantswarm/agent-platform#548",
	})
	require.NoError(t, err)
	require.Equal(t, "- Update giantswarm/pause to v3.10.2 (giantswarm/agent-platform#548)", target)
}

func TestChangelogLine_teamFormat(t *testing.T) {
	policy := ChangelogDefaults()
	policy.Template = "* {{ .Kind }}: {{ .Dependency }} {{ .To }} ({{ .UpdateType }})"

	line, err := ChangelogLine(policy, ChangelogFacts{
		Dependency: "lodash", To: "4.17.21", Kind: "renovate", UpdateType: "patch",
	})
	require.NoError(t, err)
	require.Equal(t, "* renovate: lodash 4.17.21 (patch)", line)
}

func TestChangelogLine_refusesAnUnknownValue(t *testing.T) {
	policy := ChangelogDefaults()
	policy.Template = "- {{ .Nothing }}"

	_, err := ChangelogLine(policy, ChangelogFacts{Dependency: "lodash"})
	require.Error(t, err)
}

func TestInsertChangelogLine_atTheEndOfItsSection(t *testing.T) {
	got, changed := InsertChangelogLine(keepAChangelog, ChangelogDefaults(), "- Update lodash to 4.17.21.")

	require.True(t, changed)
	require.Contains(t, got, "### Changed\n\n- Another thing.\n- Update lodash to 4.17.21.\n")
	// The released section below it is untouched.
	require.Contains(t, got, "## [1.2.0] - 2026-09-01\n\n### Added\n\n- The first thing.\n")
}

func TestInsertChangelogLine_writesOnceForTheSameLine(t *testing.T) {
	line := "- Update lodash to 4.17.21."
	once, _ := InsertChangelogLine(keepAChangelog, ChangelogDefaults(), line)

	twice, changed := InsertChangelogLine(once, ChangelogDefaults(), line)

	require.False(t, changed)
	require.Equal(t, once, twice)
}

func TestInsertChangelogLine_createsTheSectionUnderItsHeading(t *testing.T) {
	file := "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- A thing.\n"

	got, changed := InsertChangelogLine(file, ChangelogDefaults(), "- Update lodash to 4.17.21.")

	require.True(t, changed)
	require.Contains(t, got, "### Changed\n\n- Update lodash to 4.17.21.")
	require.Contains(t, got, "### Added\n\n- A thing.")
}

func TestInsertChangelogLine_createsTheHeadingUnderTheTitle(t *testing.T) {
	file := "# Changelog\n\nEverything worth knowing.\n\n## [1.2.0] - 2026-09-01\n\n### Added\n\n- The first thing.\n"

	got, changed := InsertChangelogLine(file, ChangelogDefaults(), "- Update lodash to 4.17.21.")

	require.True(t, changed)
	require.Contains(t, got, "## [Unreleased]\n\n### Changed\n\n- Update lodash to 4.17.21.")
	// The heading goes above the released one, not inside it.
	require.Less(t, strings.Index(got, "## [Unreleased]"), strings.Index(got, "## [1.2.0]"))
}

func TestInsertChangelogLine_findsADatedHeading(t *testing.T) {
	file := "# Changelog\n\n## [Unreleased] - upcoming\n\n### Changed\n\n- Another thing.\n"

	got, changed := InsertChangelogLine(file, ChangelogDefaults(), "- Update lodash to 4.17.21.")

	require.True(t, changed)
	require.Contains(t, got, "- Another thing.\n- Update lodash to 4.17.21.\n")
	require.NotContains(t, got, "## [Unreleased]\n\n### Changed\n\n- Update lodash")
}

func TestInsertChangelogLine_endsWithOneNewline(t *testing.T) {
	got, _ := InsertChangelogLine(keepAChangelog, ChangelogDefaults(), "- Update lodash to 4.17.21.")

	require.True(t, len(got) > 0 && got[len(got)-1] == '\n')
	require.NotContains(t, got, "\n\n\n\n")
}
