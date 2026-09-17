package pr

import "strings"

// LabelPrefix is the namespace of the one classification label the sweep
// keeps on every bot PR it touched. Labels are display only: no guard reads
// them back.
const LabelPrefix = "marge/"

// LegacyLabelPrefix is a second namespace the sweep recognises as its own:
// a label carrying it is removed from every PR the sweep touches, so no PR
// ends a sweep with two classifications.
const LegacyLabelPrefix = "bot-prs-sweep/"

// LabelClass returns the classification label suffix for a final state, or
// "" for a transient state that never ends a sweep.
func LabelClass(s StatusState) string {
	switch s {
	case StatusMerged, StatusAlreadyMerged:
		return "merged"
	case StatusAutoMerge:
		return "auto-merge"
	case StatusFailed, StatusHeld:
		return "action-required"
	case StatusFailedSecurity:
		return "security"
	case StatusBlockedCI, StatusNoVerdict:
		return "ci-unavailable"
	case StatusStale, StatusRefreshed:
		return "stale"
	case StatusCancelled, StatusRetried, StatusWaitingChecks:
		return "pending"
	case StatusConflict:
		return "conflict"
	case StatusAwaitingApproval:
		return "awaiting-approval"
	case StatusEligible:
		return "eligible"
	case StatusRemedied:
		return "remedied"
	case StatusSkipped, StatusUntrustedAuthor:
		return "skipped"
	default:
		return ""
	}
}

// StoredClass returns the classification label the last sweep left on a PR
// and the class it carries: its marge/ label, or its legacy label when the
// PR has not been swept since the rename. Both are "" for a PR no sweep has
// labelled, which has no stored classification to read.
func StoredClass(labels []string) (label, class string) {
	for _, prefix := range []string{LabelPrefix, LegacyLabelPrefix} {
		for _, name := range labels {
			if suffix, found := strings.CutPrefix(name, prefix); found && suffix != "" {
				return name, suffix
			}
		}
	}
	return "", ""
}

// ClassState returns the state a stored classification stands for. Several
// states share one label, so the state is the class's representative, not
// the exact state of the sweep that wrote it: "pending" comes back as
// StatusWaitingChecks whether the sweep saw a wait, a cancelled build or a
// retry. A class no state maps to reports false.
func ClassState(class string) (StatusState, bool) {
	switch class {
	case "merged":
		return StatusMerged, true
	case "auto-merge":
		return StatusAutoMerge, true
	case "action-required":
		return StatusFailed, true
	case "security":
		return StatusFailedSecurity, true
	case "ci-unavailable":
		return StatusBlockedCI, true
	case "stale":
		return StatusStale, true
	case "pending":
		return StatusWaitingChecks, true
	case "conflict":
		return StatusConflict, true
	case "awaiting-approval":
		return StatusAwaitingApproval, true
	case "eligible":
		return StatusEligible, true
	case "remedied":
		return StatusRemedied, true
	case "skipped":
		return StatusSkipped, true
	default:
		return StatusUnclassified, false
	}
}
