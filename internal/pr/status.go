package pr

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
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
	// StatusObsolete marks a bot PR that is not worth fixing: either a
	// sibling PR carries a higher version of the same dependency, or the
	// diff changes nothing that executes. Both want closing, not rescuing.
	// The entry's ObsoleteReason says which, and Detail says why.
	StatusObsolete
	// StatusWaitingChecks marks a PR whose required status checks have not
	// all reported: a required context is missing or pending. The sweep
	// waits; it never merges past a required check.
	StatusWaitingChecks
	// StatusAwaitingApproval marks a green PR that GitHub refused to merge
	// for a review reason the sweep's own approval did not satisfy.
	StatusAwaitingApproval
	// StatusHeld marks a green PR the sweep policy leaves to a person, for
	// instance a major update or one whose update type could not be read.
	StatusHeld
	// StatusEligible marks a green eligible PR the sweep did not merge
	// because the merge action was not selected.
	StatusEligible
	// StatusAwaitingRebase marks a PR the sweep's own merge of a sibling PR
	// of the same repository made dirty. The bot that opened it rebases it
	// within about a minute, so the conflict is not work for a person. The
	// second pass of the run looks at the PR again.
	StatusAwaitingRebase
	// StatusRemedied marks a PR a rule of the catalogue acted on. The
	// action's evidence names the rule and what it did; the next sweep
	// classifies the result.
	StatusRemedied
	// StatusUnclassified marks a PR that carries no classification label:
	// no sweep has decided it yet. No sweep writes this state, and no
	// label stands for it; only a read of the stored classification
	// reports it.
	StatusUnclassified
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
	case StatusObsolete:
		return "Obsolete"
	case StatusWaitingChecks:
		return "Waiting for checks"
	case StatusAwaitingApproval:
		return "Awaiting approval"
	case StatusHeld:
		return "Held"
	case StatusEligible:
		return "Eligible"
	case StatusAwaitingRebase:
		return "Awaiting rebase"
	case StatusRemedied:
		return "Remedied"
	case StatusUnclassified:
		return "Unclassified"
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
	// Kind and UpdateType are set once the PR has been read; both are
	// empty for a PR the sweep could not fetch.
	Kind       Kind
	UpdateType UpdateType
	// Label is the marge/<class> label that is on the PR after the
	// sweep; empty when none was written (dry run, or the write failed).
	Label string
	// Rescue is the most recent prior automated rescue attempt found on
	// the PR, if any. Only populated for failure-state entries.
	Rescue *RescueMarker
	// ObsoleteReason says why an obsolete PR is obsolete, for consumers
	// that dispatch on it rather than on Detail. Empty unless State is
	// StatusObsolete.
	ObsoleteReason ObsoleteReason
	// Policy is the sweep policy resolved for this PR's repository. Nil
	// for a PR the sweep could not fetch.
	Policy *Policy
	// Unhandled describes a failure no rule of the catalogue recognised.
	// Nil when a rule matched, or when the state is not one a rule acts on.
	Unhandled *Unhandled
	// Finding names the repository setting that holds this PR back. Nil
	// when no setting stands between the PR and a merge.
	Finding *RepoFinding
}

// RepoFindingCause names a repository setting the sweep cannot act on. The
// engine refuses the same PRs it refused before; the cause says why a queue
// does not drain, so the fix goes to the repository rather than to marge.
type RepoFindingCause string

const (
	// FindingStrictProtection: the base branch requires an up-to-date
	// branch, so every merge puts the sibling PRs behind their base and the
	// repository merges one PR per sweep.
	FindingStrictProtection RepoFindingCause = "strict_protection"
	// FindingCodeOwnerReview: the base branch requires an approval from a
	// code owner, which the sweep App cannot give.
	FindingCodeOwnerReview RepoFindingCause = "code_owner_review"
	// FindingSilentContext: a required status check has not reported for
	// long enough that no run will report it. The PR waits for ever.
	FindingSilentContext RepoFindingCause = "silent_required_context"
)

// SilentContextAfter is how long a required context may stay unreported on
// a settled head before the sweep reports it as a repository-settings
// problem. A context no workflow produces never reports, and the PRs that
// require it wait for ever inside a classification that means "come back
// later".
const SilentContextAfter = 7 * 24 * time.Hour

// RepoFinding is one repository setting that holds one PR back.
type RepoFinding struct {
	Cause RepoFindingCause
	// Detail carries what the cause needs to be acted on, for instance the
	// required context that never reported. Empty when the cause names
	// itself.
	Detail string
}

// SetFinding records the repository setting that holds the entry back.
func (s *PRStatus) SetFinding(idx int, finding *RepoFinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx >= 0 && idx < len(s.entries) {
		s.entries[idx].Finding = finding
	}
}

// FindingEntries returns the entries a repository setting holds back.
func (s *PRStatus) FindingEntries() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StatusEntry
	for _, e := range s.entries {
		if e.Finding != nil {
			out = append(out, e)
		}
	}
	return out
}

// ObsoleteReason names why a PR is not worth fixing.
type ObsoleteReason string

const (
	// ReasonSuperseded: a sibling PR in the same repository carries a
	// higher version of the same dependency.
	ReasonSuperseded ObsoleteReason = "superseded"
	// ReasonNoOp: the diff changes nothing that executes.
	ReasonNoOp ObsoleteReason = "no_op"
)

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

// MarkObsolete records an entry as obsolete together with the reason, so a
// consumer can dispatch on the reason instead of parsing the detail.
func (s *PRStatus) MarkObsolete(idx int, reason ObsoleteReason, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		s.entries[idx].State = StatusObsolete
		s.entries[idx].Detail = detail
		s.entries[idx].ObsoleteReason = reason
	}
}

// SetClassification records what kind of bot PR the entry is and the size
// of the update it carries.
func (s *PRStatus) SetClassification(idx int, kind Kind, updateType UpdateType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		s.entries[idx].Kind = kind
		s.entries[idx].UpdateType = updateType
	}
}

// SetPolicy records the sweep policy resolved for the entry's repository,
// so every decision the sweep took can be explained afterwards.
func (s *PRStatus) SetPolicy(idx int, policy Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		s.entries[idx].Policy = &policy
	}
}

// SetLabel records the classification label that is on the PR.
func (s *PRStatus) SetLabel(idx int, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.entries) {
		s.entries[idx].Label = label
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

// Unhandled is a failure the catalogue does not recognise yet. Its
// signature groups the same failure across PRs, so a pattern worth a rule
// is visible as a count rather than as one PR at a time.
type Unhandled struct {
	// Signature identifies the shape of the failure: the failing checks and
	// the log excerpt, normalised.
	Signature string
	// Checks are the failing checks that produced a verdict.
	Checks []string
	// Excerpt is the log excerpt a rule would match against, when one was
	// read. Empty when no rule asked for a log.
	Excerpt string
}

// SetUnhandled records that no rule recognised this PR's failure.
func (s *PRStatus) SetUnhandled(idx int, unhandled *Unhandled) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx >= 0 && idx < len(s.entries) {
		s.entries[idx].Unhandled = unhandled
	}
}

// UnhandledEntries returns the entries whose failure no rule recognised.
func (s *PRStatus) UnhandledEntries() []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StatusEntry
	for _, e := range s.entries {
		if e.Unhandled != nil {
			out = append(out, e)
		}
	}
	return out
}

// Counts holds the aggregate tallies of a sweep by outcome category.
//
// Failed covers every action-required outcome (plain and security failures,
// conflicts, untrusted authors). Blocked (CI could not run because of an
// Actions budget block), NoVerdict (the failing checks established nothing
// about the code), Stale (failing checks are green on the base branch head
// and the PR is behind it), Refreshed (a stale branch was just updated from
// its base), Cancelled (CircleCI auto-cancelled the failing builds), Retried
// (those builds were just retried) and Obsolete (a sibling PR carries a
// higher version of the same dependency, or the diff changes nothing that
// executes) are counted separately from Failed: none of them is a genuine CI
// failure and none of them belongs in the rescue path.
type Counts struct {
	Merged int
	// AutoMerge counts the PRs handed to GitHub's own auto-merge. Nothing is
	// merged yet: GitHub fires it when the last requirement is met, which can
	// be on a later sweep or never.
	AutoMerge int
	Failed    int
	Blocked   int
	NoVerdict int
	Stale     int
	Refreshed int
	Cancelled int
	Retried   int
	Obsolete  int
	Remedied  int
	Waiting   int
	Skipped   int
	// Eligible counts green PRs the sweep did not merge because the merge
	// action was not selected, which is every green PR of a classify-only
	// run.
	Eligible int
	// Unclassified counts PRs no sweep has labelled. Only a read of the
	// stored classification produces them.
	Unclassified int
}

// countsLocked tallies entries by category. Callers must hold s.mu.
func (s *PRStatus) countsLocked() Counts {
	var c Counts
	for _, e := range s.entries {
		switch e.State {
		case StatusMerged, StatusAlreadyMerged:
			c.Merged++
		case StatusAutoMerge:
			c.AutoMerge++
		case StatusFailed, StatusFailedSecurity, StatusConflict, StatusUntrustedAuthor, StatusHeld, StatusAwaitingApproval:
			c.Failed++
		case StatusBlockedCI:
			c.Blocked++
		case StatusNoVerdict:
			c.NoVerdict++
		case StatusWaitingChecks, StatusAwaitingRebase:
			c.Waiting++
		case StatusStale:
			c.Stale++
		case StatusRefreshed:
			c.Refreshed++
		case StatusCancelled:
			c.Cancelled++
		case StatusRetried:
			c.Retried++
		case StatusObsolete:
			c.Obsolete++
		case StatusRemedied:
			c.Remedied++
		case StatusSkipped:
			c.Skipped++
		case StatusEligible:
			c.Eligible++
		case StatusUnclassified:
			c.Unclassified++
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
	if c.Obsolete > 0 {
		fmt.Fprintf(&b, ", %d obsolete", c.Obsolete)
	}
	if c.Remedied > 0 {
		fmt.Fprintf(&b, ", %d remedied", c.Remedied)
	}
	if c.Blocked > 0 {
		fmt.Fprintf(&b, ", %d CI-unavailable", c.Blocked)
	}
	if c.NoVerdict > 0 {
		fmt.Fprintf(&b, ", %d no-verdict", c.NoVerdict)
	}
	if c.Waiting > 0 {
		fmt.Fprintf(&b, ", %d waiting", c.Waiting)
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
		case StatusFailed, StatusFailedSecurity, StatusConflict, StatusUntrustedAuthor, StatusHeld, StatusAwaitingApproval:
			result = append(result, e)
		}
	}
	sortOldestFirst(result)
	return result
}

// WaitingEntries returns entries whose required checks have not all
// reported, and entries waiting for the bot to rebase a conflict this sweep
// caused. They are kept out of ActionRequired: the remedy is time.
func (s *PRStatus) WaitingEntries() []StatusEntry {
	entries := s.entriesInStates(StatusWaitingChecks, StatusAwaitingRebase)
	sortOldestFirst(entries)
	return entries
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

// ObsoleteEntries returns entries that are not worth fixing: a sibling PR
// carries a higher version, or the diff changes nothing that executes. Like
// StaleEntries they are kept out of ActionRequired and the failed counts:
// the remedy is to close them, not to rescue them. Oldest PR first.
func (s *PRStatus) ObsoleteEntries() []StatusEntry {
	return s.entriesInState(StatusObsolete)
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

// RemediedEntries returns the entries a rule of the catalogue acted on in
// this run.
func (s *PRStatus) RemediedEntries() []StatusEntry {
	return s.entriesInState(StatusRemedied)
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
	return s.entriesInStates(StatusMerged, StatusAlreadyMerged)
}

// AutoMergeEntries lists the PRs left to GitHub's auto-merge.
func (s *PRStatus) AutoMergeEntries() []StatusEntry {
	return s.entriesInStates(StatusAutoMerge)
}

func (s *PRStatus) entriesInStates(states ...StatusState) []StatusEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []StatusEntry
	for _, e := range s.entries {
		if slices.Contains(states, e.State) {
			result = append(result, e)
		}
	}
	return result
}

// EligibleEntries returns the green entries the sweep left unmerged
// because the merge action was not selected.
func (s *PRStatus) EligibleEntries() []StatusEntry {
	return s.entriesInState(StatusEligible)
}

// UnclassifiedEntries returns the entries no sweep has labelled.
func (s *PRStatus) UnclassifiedEntries() []StatusEntry {
	return s.entriesInState(StatusUnclassified)
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
