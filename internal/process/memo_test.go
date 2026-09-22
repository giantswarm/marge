package process

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMemoRunsOneLookupPerKey is the property the caches of a sweep depend
// on: twenty PRs that need the same answer at the same time cost one
// request, not twenty.
//
// leaderIn is what makes it a test of that property rather than of the
// answer. Without it the callers may run one after the other, and a cache
// that releases its lock across the lookup reports one lookup too. The
// other callers ask only once the first is inside load, which is the state
// a per-call cache answers wrongly.
func TestMemoRunsOneLookupPerKey(t *testing.T) {
	var cache memo[int]
	var lookups atomic.Int32
	leaderIn := make(chan struct{})
	var entered atomic.Int32
	release := make(chan struct{})

	load := func() (int, error) {
		lookups.Add(1)
		return 7, nil
	}

	var wg sync.WaitGroup
	answers := make([]int, 20)
	wg.Go(func() {
		answers[0], _ = cache.get("one", func() (int, error) {
			lookups.Add(1)
			close(leaderIn)
			<-release
			return 7, nil
		})
	})
	for i := 1; i < len(answers); i++ {
		wg.Go(func() {
			<-leaderIn
			entered.Add(1)
			answers[i], _ = cache.get("one", load)
		})
	}

	for entered.Load() < int32(len(answers)-1) {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	require.Equal(t, int32(1), lookups.Load())
	for i := range answers {
		require.Equal(t, 7, answers[i])
	}

	_, _ = cache.get("two", func() (int, error) { lookups.Add(1); return 9, nil })
	require.Equal(t, int32(2), lookups.Load(), "a second key is its own lookup")
}

// A failed lookup caches nothing. A transient GitHub failure on the first PR
// of a repository must not become the answer every later PR of that
// repository reads.
func TestMemoDoesNotCacheAFailure(t *testing.T) {
	var cache memo[int]
	boom := errors.New("boom")
	lookups := 0

	value, err := cache.get("one", func() (int, error) {
		lookups++
		return 0, boom
	})
	require.ErrorIs(t, err, boom)
	require.Zero(t, value)

	value, err = cache.get("one", func() (int, error) {
		lookups++
		return 3, nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, value)
	require.Equal(t, 2, lookups)
}

// A caller that waited on a lookup that failed asks again itself, so one
// failure does not fail every caller that was waiting behind it.
func TestMemoRetriesAfterAWaitedFailure(t *testing.T) {
	var cache memo[int]
	var lookups atomic.Int32
	first := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	var failed, waited int
	var failedErr, waitedErr error
	wg.Go(func() {
		failed, failedErr = cache.get("one", func() (int, error) {
			lookups.Add(1)
			close(first)
			<-release
			return 0, errors.New("boom")
		})
	})
	<-first
	wg.Go(func() {
		close(release)
		waited, waitedErr = cache.get("one", func() (int, error) {
			lookups.Add(1)
			return 5, nil
		})
	})
	wg.Wait()

	require.Error(t, failedErr)
	require.Zero(t, failed)
	require.NoError(t, waitedErr)
	require.Equal(t, 5, waited)
	require.Equal(t, int32(2), lookups.Load())
}
