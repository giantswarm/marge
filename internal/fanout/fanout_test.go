package fanout

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEach_boundsTheCallsInFlight: no more than limit calls run at once,
// every index is called once, and a limit of 1 calls them in index order.
func TestEach_boundsTheCallsInFlight(t *testing.T) {
	for _, tt := range []struct {
		name   string
		limit  int
		widest int
	}{
		{name: "one after another", limit: 1, widest: 1},
		{name: "below one runs one after another", limit: -3, widest: 1},
		{name: "bounded", limit: 3, widest: 3},
		{name: "zero is the default", limit: 0, widest: Default},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const n = 20
			var (
				mu       sync.Mutex
				inFlight int
				widest   int
				order    []int
			)
			called := make([]int, n)
			Each(n, tt.limit, func(i int) {
				mu.Lock()
				inFlight++
				widest = max(widest, inFlight)
				order = append(order, i)
				mu.Unlock()

				time.Sleep(5 * time.Millisecond)
				called[i]++

				mu.Lock()
				inFlight--
				mu.Unlock()
			})

			for i, count := range called {
				require.Equal(t, 1, count, "index %d", i)
			}
			require.Equal(t, tt.widest, widest)
			if tt.widest == 1 {
				for i, index := range order {
					require.Equal(t, i, index)
				}
			}
		})
	}
}
