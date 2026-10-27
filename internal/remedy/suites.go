package remedy

import "sync"

// SuiteBudget bounds how many e2e suites one run starts. Every suite costs
// clusters on a shared test management cluster, whose Flux helm-controller
// stalls when too many of them reconcile at once, so a run that finds many
// gated PRs starts a few and leaves the rest to a later run. One budget is
// shared by every team and PR of a run. A nil budget bounds nothing.
type SuiteBudget struct {
	mu   sync.Mutex
	left int
}

// NewSuiteBudget returns a budget of limit suites, none when limit is not
// positive.
func NewSuiteBudget(limit int) *SuiteBudget {
	return &SuiteBudget{left: max(limit, 0)}
}

// Take reserves n suites, all or none, and reports whether it did. A comment
// that names several providers starts all of them, so it fits whole or not
// at all.
func (b *SuiteBudget) Take(n int) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.left {
		return false
	}
	b.left -= n
	return true
}

// Give returns n suites a Take reserved for a comment that was never posted.
func (b *SuiteBudget) Give(n int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.left += n
}
