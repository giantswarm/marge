package rules

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeCatalogue(t *testing.T, docs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range docs {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	return dir
}

func ruleDoc(name, action string) string {
	return "name: " + name + `
summary: s
source: runbook row 1
match:
  check:
    name: "go-build"
  pr:
    baseHead: absent
action:
  name: ` + action + `
evidence:
  reason: y
`
}

func TestLoadOrdersRulesByName(t *testing.T) {
	dir := writeCatalogue(t, map[string]string{
		"update-behind.yaml":   ruleDoc("update-behind", "update-branch"),
		"close-obsolete.yaml":  ruleDoc("close-obsolete", "close"),
		"retry-cancelled.yaml": ruleDoc("retry-cancelled", "circleci-retry"),
		"notes.md":             "not a rule",
	})

	cat, err := Loader{LocalPath: dir}.Load(t.Context(), testRegistry())
	require.NoError(t, err)

	require.Empty(t, cat.Skipped)
	names := make([]string, len(cat.Rules))
	for i, r := range cat.Rules {
		names[i] = r.Name
	}
	require.Equal(t, []string{"close-obsolete", "retry-cancelled", "update-behind"}, names)
}

// One unusable document costs its own rule and nothing more: the rest of the
// catalogue still runs, so a single bad file cannot stop every remedy.
func TestLoadSkipsAnInvalidDocument(t *testing.T) {
	dir := writeCatalogue(t, map[string]string{
		"good-rule.yaml": ruleDoc("good-rule", "close"),
		"bad-rule.yaml":  ruleDoc("bad-rule", "merge-everything"),
	})

	cat, err := Loader{LocalPath: dir}.Load(t.Context(), testRegistry())
	require.NoError(t, err)

	require.Len(t, cat.Rules, 1)
	require.Equal(t, "good-rule", cat.Rules[0].Name)
	require.Len(t, cat.Skipped, 1)
	require.Equal(t, "bad-rule.yaml", cat.Skipped[0].Path)
	require.Contains(t, cat.Skipped[0].Reason, "unknown action")
}

func TestLoadDigestChangesWithContent(t *testing.T) {
	first := writeCatalogue(t, map[string]string{"close-obsolete.yaml": ruleDoc("close-obsolete", "close")})
	same := writeCatalogue(t, map[string]string{"close-obsolete.yaml": ruleDoc("close-obsolete", "close")})
	other := writeCatalogue(t, map[string]string{"close-obsolete.yaml": ruleDoc("close-obsolete", "update-branch")})

	load := func(dir string) string {
		cat, err := Loader{LocalPath: dir}.Load(t.Context(), testRegistry())
		require.NoError(t, err)
		return cat.Digest
	}

	require.Equal(t, load(first), load(same))
	require.NotEqual(t, load(first), load(other))
}

func TestLoadMissingDirectory(t *testing.T) {
	_, err := Loader{LocalPath: filepath.Join(t.TempDir(), "absent")}.Load(t.Context(), testRegistry())
	require.ErrorContains(t, err, "reading rule directory")
}

func TestCatalogueAvailable(t *testing.T) {
	var missing *Catalogue
	require.False(t, missing.Available())

	dir := writeCatalogue(t, map[string]string{"close-obsolete.yaml": ruleDoc("close-obsolete", "close")})
	cat, err := Loader{LocalPath: dir}.Load(t.Context(), testRegistry())
	require.NoError(t, err)
	require.True(t, cat.Available())
}

// TestCatalogue_Only narrows the catalogue to one rule. The rule still has
// to match: narrowing removes the other rules from the contest and nothing
// more.
func TestCatalogue_Only(t *testing.T) {
	catalogue := &Catalogue{
		Source: "giantswarm/marge@main:rules",
		Digest: "a-digest",
		Rules:  []*Rule{{Name: "first"}, {Name: "second"}},
	}

	narrowed, err := catalogue.Only("second")
	require.NoError(t, err)
	require.Len(t, narrowed.Rules, 1)
	require.Equal(t, "second", narrowed.Rules[0].Name)
	require.Equal(t, catalogue.Digest, narrowed.Digest, "the evidence still names the catalogue the rule came from")
	require.Equal(t, catalogue.Source, narrowed.Source)
	require.Len(t, catalogue.Rules, 2, "the catalogue it was narrowed from is unchanged")

	_, err = catalogue.Only("absent")
	require.ErrorContains(t, err, "unknown rule")

	_, err = (*Catalogue)(nil).Only("first")
	require.ErrorContains(t, err, "no rule catalogue")
}
