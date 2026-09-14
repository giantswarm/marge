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
