package process

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// The hold the label reports is explained on the PR, with the files whose
// policy decided it: the label alone says a person must act and never why.
func TestHeldMarkerReasonNamesTheUpdateTypeAndThePolicyFiles(t *testing.T) {
	policy := pr.Policy{Sources: []string{"bot-prs-sweep/default.yaml", "bot-prs-sweep/team-shield.yaml"}}

	reason := heldMarkerReason(pr.KindRenovate, pr.UpdateMajor, policy)

	require.Contains(t, reason, "major update waits for a person")
	require.Contains(t, reason, "bot-prs-sweep/default.yaml")
	require.Contains(t, reason, "bot-prs-sweep/team-shield.yaml")
	require.NotContains(t, reason, "held: ")
}

// An unreadable update type is its own reason, and still names the policy.
func TestHeldMarkerReasonCarriesAnUnreadableUpdateType(t *testing.T) {
	policy := pr.Policy{Sources: []string{"built-in company defaults"}}

	reason := heldMarkerReason(pr.KindDependabot, pr.UpdateUnknown, policy)

	require.Contains(t, reason, "could not be read")
	require.Contains(t, reason, "built-in company defaults")
}

// A policy with no source named leaves the reason alone rather than
// trailing an empty list.
func TestHeldMarkerReasonWithoutSourcesIsTheReasonAlone(t *testing.T) {
	reason := heldMarkerReason(pr.KindRenovate, pr.UpdateMajor, pr.Policy{})

	require.Equal(t, heldReason(pr.KindRenovate, pr.UpdateMajor), reason)
}

// The status detail keeps the word the state is reported under, so the
// run's own output does not change with the marker.
func TestHeldDetailKeepsItsPrefix(t *testing.T) {
	require.Equal(t, "held: "+heldReason(pr.KindRenovate, pr.UpdateMajor), heldDetail(pr.KindRenovate, pr.UpdateMajor))
}
