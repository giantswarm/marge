package process

import (
	"fmt"
	"strings"

	"github.com/giantswarm/marge/internal/pr"
)

// heldReason names what waits for a person, without the state's own word in
// front of it.
func heldReason(kind pr.Kind, updateType pr.UpdateType) string {
	if updateType == pr.UpdateUnknown {
		return fmt.Sprintf("update type of this %s PR could not be read", kind)
	}
	return fmt.Sprintf("%s update waits for a person under the resolved policy", updateType)
}

// heldDetail explains why a green PR is left to a person.
func heldDetail(kind pr.Kind, updateType pr.UpdateType) string {
	return "held: " + heldReason(kind, updateType)
}

// needsProofReason explains a PR held because its body asks for a proof
// before merging, naming the first open item of that checklist.
func needsProofReason(item string) string {
	const maxItem = 80
	if r := []rune(item); len(r) > maxItem {
		item = string(r[:maxItem]) + "…"
	}
	if item == "" {
		return "needs proof: the PR body asks for a proof before merging"
	}
	return fmt.Sprintf("needs proof: the PR body asks for a proof before merging (open: %q)", item)
}

// heldMarkerReason explains the same hold on the PR itself, and names the
// files the resolved policy came from. A person who reads the label alone
// knows a hold happened and not which key held it, and the run's log is
// gone with the run.
func heldMarkerReason(kind pr.Kind, updateType pr.UpdateType, policy pr.Policy) string {
	reason := heldReason(kind, updateType)
	if len(policy.Sources) == 0 {
		return reason
	}
	return reason + "; policy from " + strings.Join(policy.Sources, ", ")
}
