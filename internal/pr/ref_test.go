package pr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParsePRRef covers both spellings a caller uses to name one PR: the
// browser URL, and the owner/repo#number shorthand a sweep reports.
func TestParsePRRef(t *testing.T) {
	tests := []struct {
		ref    string
		owner  string
		repo   string
		number int
	}{
		{"https://github.com/giantswarm/happa/pull/4808", "giantswarm", "happa", 4808},
		{"giantswarm/happa#4808", "giantswarm", "happa", 4808},
		{"  giantswarm/happa#4808  ", "giantswarm", "happa", 4808},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			owner, repo, number, err := ParsePRRef(tt.ref)
			require.NoError(t, err)
			require.Equal(t, tt.owner, owner)
			require.Equal(t, tt.repo, repo)
			require.Equal(t, tt.number, number)
		})
	}

	for _, ref := range []string{"", "giantswarm/happa", "giantswarm#4808", "/happa#4808", "giantswarm/happa#", "giantswarm/happa#zero", "giantswarm/happa#0", "a/b/c#1", "https://github.com/giantswarm/happa"} {
		t.Run("refused: "+ref, func(t *testing.T) {
			_, _, _, err := ParsePRRef(ref)
			require.Error(t, err)
		})
	}
}

// TestKey compares the way GitHub does: owner and repository names are
// case-insensitive, the number is not part of that.
func TestKey(t *testing.T) {
	require.Equal(t, "giantswarm/marge#7", Key("GiantSwarm", "Marge", 7))
	require.Equal(t, Key("giantswarm", "marge", 7), Key("GIANTSWARM", "MARGE", 7))
}
