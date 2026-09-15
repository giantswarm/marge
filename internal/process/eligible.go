package process

import (
	"fmt"

	"github.com/giantswarm/marge/internal/pr"
)

// heldDetail explains why a green PR is left to a person.
func heldDetail(kind pr.Kind, updateType pr.UpdateType) string {
	if updateType == pr.UpdateUnknown {
		return fmt.Sprintf("held: update type of this %s PR could not be read", kind)
	}
	return fmt.Sprintf("held: %s update waits for a person under the resolved policy", updateType)
}
