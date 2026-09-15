package process

import (
	"fmt"

	"github.com/giantswarm/marge/internal/pr"
)

// eligible applies the company default sweep policy: Align files and Herald
// PRs merge whenever green; Renovate and Dependabot merge everything below
// a major. A major, or an update whose size could not be read, waits for a
// person.
func eligible(kind pr.Kind, updateType pr.UpdateType) bool {
	switch kind {
	case pr.KindAlignFiles, pr.KindHerald:
		return true
	}
	switch updateType {
	case pr.UpdatePatch, pr.UpdateMinor, pr.UpdateDigest, pr.UpdatePin, pr.UpdateLockfile:
		return true
	}
	return false
}

// heldDetail explains why a green PR is left to a person.
func heldDetail(kind pr.Kind, updateType pr.UpdateType) string {
	if updateType == pr.UpdateUnknown {
		return fmt.Sprintf("held: update type of this %s PR could not be read", kind)
	}
	return fmt.Sprintf("held: %s update waits for a person", updateType)
}
