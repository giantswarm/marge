package pr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStoredClass_readsTheOneLabelASweepLeaves: a PR carries at most one
// classification label, in either namespace, among labels the sweep does
// not own.
func TestStoredClass_readsTheOneLabelASweepLeaves(t *testing.T) {
	tests := []struct {
		name      string
		labels    []string
		wantLabel string
		wantClass string
	}{
		{"no label at all", nil, "", ""},
		{"no label of the sweep", []string{"dependencies", "renovate"}, "", ""},
		{"the current namespace", []string{"dependencies", "marge/action-required"}, "marge/action-required", "action-required"},
		{"the legacy namespace", []string{"bot-prs-sweep/merged"}, "bot-prs-sweep/merged", "merged"},
		{"the current namespace wins", []string{"bot-prs-sweep/merged", "marge/stale"}, "marge/stale", "stale"},
		{"the bare prefix classifies nothing", []string{"marge/"}, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, class := StoredClass(tt.labels)
			require.Equal(t, tt.wantLabel, label)
			require.Equal(t, tt.wantClass, class)
		})
	}
}

// TestClassState_readsBackEveryClassASweepWrites is the contract between the
// two directions: every class a sweep can write reads back as a state that
// writes that same class. The state is the class's representative, so the
// round trip holds on the class, not on the state.
func TestClassState_readsBackEveryClassASweepWrites(t *testing.T) {
	for state := StatusPending; state <= StatusUnclassified; state++ {
		class := LabelClass(state)
		if class == "" {
			continue
		}
		t.Run(class, func(t *testing.T) {
			got, classified := ClassState(class)
			require.True(t, classified, "class %q is written but cannot be read back", class)
			require.Equal(t, class, LabelClass(got), "state %v does not stand for class %q", got, class)
		})
	}
}

// TestClassState_refusesWhatNoSweepWrote keeps a label nobody owns from
// becoming a classification.
func TestClassState_refusesWhatNoSweepWrote(t *testing.T) {
	for _, class := range []string{"", "obsolete", "whatever"} {
		state, classified := ClassState(class)
		require.False(t, classified, "class %q must not classify", class)
		require.Equal(t, StatusUnclassified, state)
	}
}
