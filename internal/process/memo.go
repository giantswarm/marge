package process

import "sync"

// memo holds one answer per key for the lifetime of a sweep, and runs one
// lookup per key however many PRs ask for it at once. The PRs of a sweep run
// together on one Processor, up to the policy's own bound, so a cache that
// only collapses repeated reads in sequence would still send one request per
// PR for the answer they share.
//
// A lookup that fails caches nothing, so a transient failure on the first PR
// is not the answer every later PR reads. The callers that waited on it ask
// again, one at a time.
type memo[V any] struct {
	mu      sync.Mutex
	entries map[string]*memoEntry[V]
}

// memoEntry is one lookup. done is closed when value and err are written,
// and neither is read before that.
type memoEntry[V any] struct {
	done  chan struct{}
	value V
	err   error
}

// get returns the answer for key, running load only for the caller that
// claims the key. Every other caller waits for that lookup and reads its
// answer.
func (m *memo[V]) get(key string, load func() (V, error)) (V, error) {
	for {
		m.mu.Lock()
		if m.entries == nil {
			m.entries = make(map[string]*memoEntry[V])
		}
		if entry, claimed := m.entries[key]; claimed {
			m.mu.Unlock()
			<-entry.done
			if entry.err == nil {
				return entry.value, nil
			}
			continue
		}
		entry := &memoEntry[V]{done: make(chan struct{})}
		m.entries[key] = entry
		m.mu.Unlock()

		entry.value, entry.err = load()
		if entry.err != nil {
			m.mu.Lock()
			delete(m.entries, key)
			m.mu.Unlock()
		}
		close(entry.done)
		return entry.value, entry.err
	}
}
