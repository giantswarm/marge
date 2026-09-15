package pr

// LabelPrefix is the namespace of the one classification label the sweep
// keeps on every bot PR it touched. Labels are display only: no guard reads
// them back.
const LabelPrefix = "bot-prs-sweep/"

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
	case StatusSkipped, StatusUntrustedAuthor:
		return "skipped"
	default:
		return ""
	}
}
