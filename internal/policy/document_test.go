package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestParseDocument_failsLoudly holds the rule that matters most about a
// policy file: a file the sweep cannot read is an error. Falling back to
// the defaults would sweep a team's repositories under a policy the team
// never wrote.
func TestParseDocument_failsLoudly(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "not YAML",
			content: "updateTypes: [unclosed\n",
			want:    "parsing policy file",
		},
		{
			name:    "misspelled key",
			content: "shedule: enabled\n",
			want:    "field shedule not found",
		},
		{
			// Decoding the first document alone would drop the rest of
			// the file without a word, and a team would read back a
			// policy its file does not describe.
			name:    "a second YAML document",
			content: "modelConfig: a\n---\nmodelConfig: b\n",
			want:    "more than one YAML document",
		},
		{
			name:    "misspelled key of a section",
			content: "rescue:\n  wekly: 3\n",
			want:    "field wekly not found",
		},
		{
			name:    "unknown bot PR kind",
			content: "updateTypes:\n  renovatebot: [patch]\n",
			want:    `unknown bot PR kind "renovatebot": known kinds are align-files, dependabot, herald, renovate`,
		},
		{
			name:    "unknown update type",
			content: "updateTypes:\n  renovate: [teeny]\n",
			want:    `updateTypes.renovate: unknown update type "teeny"`,
		},
		{
			name:    "an unreadable update can never be declared eligible",
			content: "updateTypes:\n  renovate: [patch, unknown]\n",
			want:    `unknown update type "unknown"`,
		},
		{
			name:    "timeout is not a duration",
			content: "rescue:\n  timeout: soon\n",
			want:    `rescue.timeout: "soon" is not a duration such as 20m`,
		},
		{
			name:    "timeout is not positive",
			content: "rescue:\n  timeout: 0s\n",
			want:    "rescue.timeout: 0s is not a positive duration",
		},
		{
			name:    "weekly cap is negative",
			content: "rescue:\n  weekly: -1\n",
			want:    "rescue.weekly: -1 is negative",
		},
		{
			name:    "budget is negative",
			content: "rescue:\n  budget:\n    weekly: -5\n",
			want:    "rescue.budget.weekly: -5 is negative",
		},
		{
			name:    "confirmation is neither value",
			content: "rescue:\n  confirm: maybe\n",
			want:    `rescue.confirm: "maybe" is neither per-pr nor per-sweep`,
		},
		{
			name:    "concurrency below one",
			content: "concurrency:\n  perTeam: 0\n",
			want:    "concurrency.perTeam: 0 is outside 1 to 20",
		},
		{
			name:    "concurrency above the ceiling",
			content: "concurrency:\n  perTeam: 100\n",
			want:    "concurrency.perTeam: 100 is outside 1 to 20",
		},
		{
			name:    "PRs of one repository above the ceiling",
			content: "concurrency:\n  perRepo: 10\n",
			want:    "concurrency.perRepo: 10 is outside 1 to 5",
		},
		{
			name:    "a list where a mapping belongs",
			content: "rescue:\n  - enabled: true\n",
			want:    "parsing policy file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ParseDocument(DefaultFile, tt.content)
			require.Nil(t, doc)
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, DefaultFile, "the error names the file the team must fix")
		})
	}
}

// TestParseDocument_empty accepts a file that says nothing: every key is
// optional and an absent key keeps the company default.
func TestParseDocument_empty(t *testing.T) {
	for _, content := range []string{"", "\n", "# only a comment\n"} {
		doc, err := ParseDocument(TeamFile("bumblebee"), content)
		require.NoError(t, err)
		require.NotNil(t, doc)
		require.Nil(t, doc.Rescue)
	}
}

// TestParseDocument_rescueTimeout pins the range of durations a policy file
// may write, which bot-prs-sweep/policy.schema.json in giantswarm/github
// mirrors as a pattern: every duration time.ParseDuration reads, above zero.
func TestParseDocument_rescueTimeout(t *testing.T) {
	accepted := map[string]time.Duration{
		"20m":   20 * time.Minute,
		"1.5h":  90 * time.Minute,
		"0.5h":  30 * time.Minute,
		"1h30m": 90 * time.Minute,
		"90us":  90 * time.Microsecond,
		"+20m":  20 * time.Minute,
	}
	for value, want := range accepted {
		t.Run(value, func(t *testing.T) {
			doc, err := ParseDocument(TeamFile("bumblebee"), "rescue:\n  timeout: \""+value+"\"\n")
			require.NoError(t, err)
			require.Equal(t, want, doc.Rescue.timeout)
		})
	}
}

// TestParseRepositories_failsLoudly covers the exception half: a malformed
// or widening exception on a repository entry stops the sweep, and the
// error names the repository.
func TestParseRepositories_failsLoudly(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "misspelled exception key",
			content: "- name: marge\n  botPRsSweep:\n    disabled: true\n",
			want:    "field disabled not found",
		},
		{
			name:    "unknown update type in an exception",
			content: "- name: marge\n  botPRsSweep:\n    updateTypes: [teeny]\n",
			want:    `botPRsSweep.updateTypes of repository giantswarm/marge: unknown update type "teeny"`,
		},
		{
			name:    "none in an exception",
			content: "- name: marge\n  botPRsSweep:\n    updateTypes: [patch, none]\n",
			want:    `botPRsSweep.updateTypes of repository giantswarm/marge: "none" reaches no kind an exception covers`,
		},
		{
			name:    "not a repository list",
			content: "name: marge\n",
			want:    "parsing team file",
		},
		{
			name:    "no repository at all",
			content: "- system: agent-platform\n",
			want:    "lists no repositories",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := RepositoriesFile("bumblebee")
			repos, exceptions, err := ParseRepositories(tt.content, "giantswarm", path)
			require.Nil(t, repos)
			require.Nil(t, exceptions)
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, path)
		})
	}
}

// TestParseRepositories_otherKeysAreIgnored proves the sweep reads only the
// name and its own key. Every other key of a repository entry belongs to
// the generators and changes without notice.
func TestParseRepositories_otherKeysAreIgnored(t *testing.T) {
	content := `
- name: marge
  componentType: cli
  choreReviewers: [someone]
  gen:
    flavours: [cli]
    language: go
`
	repos, exceptions, err := ParseRepositories(content, "giantswarm", RepositoriesFile("bumblebee"))
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/marge"}, repos)
	require.Empty(t, exceptions)
}
