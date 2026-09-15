package pr

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type StatusState int

const (
	StatusPending StatusState = iota
	StatusChecking
	StatusApproving
	StatusMerging
	StatusRetrying
	StatusMerged
	StatusAlreadyMerged
	StatusAutoMerge
	StatusFailed
	StatusFailedSecurity
	StatusBlockedCI
	StatusSkipped
	StatusConflict
	StatusUntrustedAuthor
	// StatusStale marks a failing PR whose head is behind its base branch
	// and whose every failing check is green on the base branch head: the
	// failure was most likely fixed on the base branch after the PR's last
	// build, so the first move is to refresh the branch, not to rescue it.
	StatusStale
	// StatusRefreshed marks a stale PR whose branch was just updated from
	// its base (the "Update branch" button); CI is running again and the
	// next sweep decides.
	StatusRefreshed
	// StatusCancelled marks a failing PR whose every failing check is a
	// CircleCI build that CircleCI itself cancelled (a newer pipeline on the
	// branch, a redundant workflow) rather than one that failed a step.
	// There is no verdict on the code yet: the remedy is a retry, not a
	// rescue.
	StatusCancelled
	// StatusRetried marks a cancelled PR whose builds were just retried on
	// the same commit; CI is running again and the next sweep decides.
	StatusRetried
	// StatusNoVerdict marks a failing PR whose every failing check
	// established nothing about the code: the job was cancelled, or a
	// CircleCI project setting refused the pipeline. There is nothing to
	// rescue, and a security check in this shape is not a finding. The
	// detail names the remedy for each check.
	StatusNoVerdict
)

func (s StatusState) String() string {
	switch s {
	case StatusPending:
		return "Pending"
	case StatusChecking:
		return "Checking CI"
	case StatusApproving:
		return "Approving"
	case StatusMerging:
		return "Merging"
	case StatusRetrying:
		return "Retrying merge"
	case StatusMerged:
		return "Merged"
	case StatusAlreadyMerged:
		return "Already merged"
	case StatusAutoMerge:
		return "Auto-merge"
	case StatusFailed:
		return "Failed"
	case StatusFailedSecurity:
		return "Failed (security)"
	case StatusBlockedCI:
		return "CI unavailable (budget)"
	case StatusSkipped:
		return "Skipped"
	case StatusConflict:
		return "Conflict"
	case StatusUntrustedAuthor:
		return "Untrusted author"
	case StatusStale:
		return "Stale"
	case StatusRefreshed:
		return "Refreshed"
	case StatusCancelled:
		return "Cancelled"
	case StatusRetried:
		return "Retried"
	case StatusNoVerdict:
		return "CI unavailable (no verdict)"
	default:
		return "Unknown"
	}
}

type PRStatus struct {
	mu      sync.Mutex
	entries []StatusEntry
}

type StatusEntry struct {
	PR     PRInfo
	State  StatusState
	Detail string
	// Rescue is the most recent prior automated rescue attempt found on
	// the PR, if any. Only populated for failure-state entries.
	Rescue *RescueMarker
}

func NewPRStatus() *PRStatus {
	return &PRStatus{}
}

func (s *PRStatus) Add(pr PRInfo) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := len(s.entries)
	s.entries = append(s.entries, StatusEntry{
		PR:    pr,
		State: StatusPending,
	})
	return idx
}

func (s *PRStatus) Update(idx int, state StatusState, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		s.entries[idx].State = state
		s.entries[idx].Detail = detail
	}
}

// SetRescue attaches a prior rescue-attempt marker to an entry.
func (s *PRStatus) SetRescue(idx int, marker *RescueMarker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		s.entries[idx].Rescue = marker
	}
}

// RescueAt returns the rescue marker attached to the entry at idx, or nil
// when none is attached or idx is out of range.
func (s *PRStatus) RescueAt(idx int) *RescueMarker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		return s.entries[idx].Rescue
	}
	return nil
}

// StateAt returns the current state of the entry at idx, or
// StatusPending when idx is out of range.
func (s *PRStatus) StateAt(idx int) StatusState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		return s.entries[idx].State
	}
	return StatusPending
}

func (s *PRStatus) Snapshot() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := make([]StatusEntry, len(s.entries))
	copy(snap, s.entries)
	return snap
}

// Counts holds the aggregate tallies of a sweep by outcome category.
//
// Failed covers every action-required outcome (plain and security failures,
// conflicts, untrusted authors). Blocked (CI could not run because of an
// Actions budget block), NoVerdict (the failing checks established nothing
// about the code), Stale (failing checks are green on the base branch head
// and the PR is behind it), Refreshed (a stale branch was just updated from
// its base), Cancelled (CircleCI auto-cancelled the failing builds) and
// Retried (those builds were just retried) are counted separately from
// Failed: none of them is a genuine CI failure and none of them belongs in
// the rescue path.
type Counts struct {
	Merged    int
	Failed    int
	Blocked   int
	NoVerdict int
	Stale     int
	Refreshed int
	Cancelled int
	Retried   int
	Skipped   int
}

// countsLocked tallies entries by category. Callers must hold s.mu.
func (s *PRStatus) countsLocked() Counts {
	var c Counts
	for _, e := range s.entries {
		switch e.State {
		case StatusMerged, StatusAlreadyMerged, StatusAutoMerge:
			c.Merged++
		case StatusFailed, StatusFailedSecurity, StatusConflict, StatusUntrustedAuthor:
			c.Failed++
		case StatusBlockedCI:
			c.Blocked++
		case StatusNoVerdict:
			c.NoVerdict++
		case StatusStale:
			c.Stale++
		case StatusRefreshed:
			c.Refreshed++
		case StatusCancelled:
			c.Cancelled++
		case StatusRetried:
			c.Retried++
		case StatusSkipped:
			c.Skipped++
		}
	}
	return c
}

// Summary returns aggregate counts across all entries.
func (s *PRStatus) Summary() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countsLocked()
}

func (s *PRStatus) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *PRStatus) FormatSummary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.countsLocked()
	var b strings.Builder
	fmt.Fprintf(&b, "%d PRs processed: %d merged, %d failed", len(s.entries), c.Merged, c.Failed)
	if c.Stale > 0 {
		fmt.Fprintf(&b, ", %d stale", c.Stale)
	}
	if c.Refreshed > 0 {
		fmt.Fprintf(&b, ", %d refreshed", c.Refreshed)
	}
	if c.Cancelled > 0 {
		fmt.Fprintf(&b, ", %d cancelled", c.Cancelled)
	}
	if c.Retried > 0 {
		fmt.Fprintf(&b, ", %d retried", c.Retried)
	}
	if c.Blocked > 0 {
		fmt.Fprintf(&b, ", %d CI-unavailable", c.Blocked)
	}
	if c.NoVerdict > 0 {
		fmt.Fprintf(&b, ", %d no-verdict", c.NoVerdict)
	}
	fmt.Fprintf(&b, ", %d skipped", c.Skipped)
	return b.String()
}

// ActionRequired returns the failure entries, oldest PR first: the
// longer a dependency PR has been open, the more sweeps it has already
// survived, so the old ones are the most likely to need manual work.
// Entries without a known creation time sort last, in insertion order.
func (s *PRStatus) ActionRequired() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []StatusEntry
	for _, e := range s.entries {
		switch e.State {
		case StatusFailed, StatusFailedSecurity, StatusConflict, StatusUntrustedAuthor:
			result = append(result, e)
		}
	}
	sortOldestFirst(result)
	return result
}

// BlockedEntries returns entries whose CI could not run because a GitHub
// Actions budget / spending-limit block prevented every job from starting.
// These are deliberately kept out of ActionRequired and the failed counts:
// the remedy is "raise or await the Actions budget", not "rescue the code".
func (s *PRStatus) BlockedEntries() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []StatusEntry
	for _, e := range s.entries {
		if e.State == StatusBlockedCI {
			result = append(result, e)
		}
	}
	return result
}

// NoVerdictEntries returns entries whose every failing check established
// nothing about the code. Like BlockedEntries they are kept out of
// ActionRequired and the failed counts: there is nothing to rescue, and each
// entry's detail names its own remedy. Oldest PR first.
func (s *PRStatus) NoVerdictEntries() []StatusEntry {
	return s.entriesInState(StatusNoVerdict)
}

// StaleEntries returns entries whose failure is stale: the PR head is behind
// its base branch and every failing check is green on the base branch head.
// Like BlockedEntries these are kept out of ActionRequired and the failed
// counts -- the remedy is "update the branch and let CI re-run", not "rescue
// the code". Oldest PR first, like ActionRequired.
func (s *PRStatus) StaleEntries() []StatusEntry {
	return s.entriesInState(StatusStale)
}

// RefreshedEntries returns the stale entries whose branch was updated from
// its base during this run. Their CI is running again; the next sweep
// decides what they are.
func (s *PRStatus) RefreshedEntries() []StatusEntry {
	return s.entriesInState(StatusRefreshed)
}

// CancelledEntries returns entries whose every failing check is a CircleCI
// build that CircleCI itself cancelled. Like StaleEntries these are kept
// out of ActionRequired and the failed counts -- the remedy is "retry the
// build and read the real verdict", not "rescue the code". Oldest PR first.
func (s *PRStatus) CancelledEntries() []StatusEntry {
	return s.entriesInState(StatusCancelled)
}

// RetriedEntries returns the cancelled entries whose builds were retried
// during this run. Their CI is running again; the next sweep decides what
// they are.
func (s *PRStatus) RetriedEntries() []StatusEntry {
	return s.entriesInState(StatusRetried)
}

func (s *PRStatus) entriesInState(state StatusState) []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []StatusEntry
	for _, e := range s.entries {
		if e.State == state {
			result = append(result, e)
		}
	}
	sortOldestFirst(result)
	return result
}

// sortOldestFirst orders entries by PR creation time, oldest first. Entries
// without a known creation time sort last, in insertion order.
func sortOldestFirst(entries []StatusEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		ci, cj := entries[i].PR.CreatedAt, entries[j].PR.CreatedAt
		if ci.IsZero() || cj.IsZero() {
			return !ci.IsZero()
		}
		return ci.Before(cj)
	})
}

// SecurityFailedEntries returns entries that failed specifically because a
// security-related check reported a problem.
func (s *PRStatus) SecurityFailedEntries() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []StatusEntry
	for _, e := range s.entries {
		if e.State == StatusFailedSecurity {
			result = append(result, e)
		}
	}
	return result
}

// SplitActionRequired partitions the action-required list into security
// failures and everything else, preserving the input order in each group.
func SplitActionRequired(entries []StatusEntry) (security, other []StatusEntry) {
	for _, e := range entries {
		if e.State == StatusFailedSecurity {
			security = append(security, e)
		} else {
			other = append(other, e)
		}
	}
	return
}

func (s *PRStatus) MergedEntries() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []StatusEntry
	for _, e := range s.entries {
		switch e.State {
		case StatusMerged, StatusAlreadyMerged, StatusAutoMerge:
			result = append(result, e)
		}
	}
	return result
}

func (s *PRStatus) SkippedEntries() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []StatusEntry
	for _, e := range s.entries {
		if e.State == StatusSkipped {
			result = append(result, e)
		}
	}
	return result
}
