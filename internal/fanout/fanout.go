// Package fanout runs the independent pieces of a sweep at once, under a
// bound.
package fanout

import "sync"

// Default is how many independent pieces of one sweep run at once when the
// caller names no bound: teams, searches, rule documents. Each piece fans
// out further on its own, and the transport bounds the requests in flight,
// so this bounds the work started, not the load on GitHub.
const Default = 8

// Each calls fn for every index below n, at most limit at once, and returns
// once every call has returned. A limit of zero means Default; 1, or less,
// runs the calls one after another, in index order. fn writes its result to
// its own index, so the caller reads the results in index order whatever
// order they finished in.
func Each(n, limit int, fn func(i int)) {
	if limit == 0 {
		limit = Default
	}
	if limit <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, limit)
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			fn(i)
		})
	}
	wg.Wait()
}
