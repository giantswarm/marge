package process

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMemoRunsOneLookupPerKey is the property the caches of a sweep depend
// on: twenty PRs that need the same answer at the same time cost one
// request, not twenty.
func TestMemoRunsOneLookupPerKey(t *testing.T) {
	var cache memo[int]
	var lookups atomic.Int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	answers := make([]int, 20)
	for i := range answers {
		wg.Go(func() {
			answers[i], _ = cache.get("one", func() (int, error) {
				lookups.Add(1)
				<-release
				return 7, nil
			})
		})
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
