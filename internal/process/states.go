package process

// GitHub commit status states and check-run fields marge decides on. Commit
// statuses report state (success, failure, error, pending); check runs report
// status (completed or not) and, once completed, a conclusion that reuses the
// success/failure words.
const (
	stateSuccess    = "success"
	stateFailure    = "failure"
	stateError      = "error"
	statePending    = "pending"
	statusCompleted = "completed"
)

// Synthetic combined-check states marge derives when every failing check
// turns out not to be a code failure. They never come from the API.
const (
	// stateBlockedBudget: an Actions budget block kept every job from
	// starting (see budget.go).
	stateBlockedBudget = "blocked"
	// stateNoVerdict: every failing check established nothing about the
	// code (see infra.go).
	stateNoVerdict = "no-verdict"
)
