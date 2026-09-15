package cmd

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// groupsOf builds repos repository groups of perRepo PRs each.
func groupsOf(repos, prsPerRepo int) []pr.PRGroup {
	groups := make([]pr.PRGroup, 0, repos)
	for r := range repos {
		name := fmt.Sprintf("repo-%d", r)
		prs := make([]pr.PRInfo, 0, prsPerRepo)
		for n := range prsPerRepo {
			prs = append(prs, pr.PRInfo{Owner: "o", Repo: name, Number: n + 1})
		}
		groups = append(groups, pr.PRGroup{Key: "o/" + name, PRs: prs})
	}
	return groups
}

// tracker records the highest number of repositories that ran at once and
// the highest number of PRs of any single repository.
type tracker struct {
	mu          sync.Mutex
	liveRepos   map[string]int
	peakRepos   int
	peakPerRepo int
	seen        atomic.Int64
}

func (t *tracker) enter(repo string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.liveRepos == nil {
		t.liveRepos = map[string]int{}
	}
	t.liveRepos[repo]++
	t.peakRepos = max(t.peakRepos, len(t.liveRepos))
	t.peakPerRepo = max(t.peakPerRepo, t.liveRepos[repo])
}

func (t *tracker) leave(repo string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.liveRepos[repo]--; t.liveRepos[repo] == 0 {
		delete(t.liveRepos, repo)
	}
}

// TestForEachPR_perTeamBoundsRepositories is the regression guard for the
// meaning of perTeam. perTeam bounds the repositories that run at once, not
// the PRs they hold together. With perTeam 5 and perRepo 2 all 10 PRs of 5
// repositories run together, so every one of them reaches the barrier. One
// semaphore taken per PR caps the run at 5 and the barrier never opens.
func TestForEachPR_perTeamBoundsRepositories(t *testing.T) {
	const repos, perRepo = 5, 2

	var seen tracker
	arrived := make(chan struct{}, repos*perRepo)
	release := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		forEachPR(groupsOf(repos, perRepo), pr.Concurrency{PerTeam: repos, PerRepo: perRepo}, func(info pr.PRInfo) {
			seen.enter(info.Repo)
			seen.seen.Add(1)
			arrived <- struct{}{}
			<-release
			seen.leave(info.Repo)
		})
	}()

	for i := range repos * perRepo {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d PRs ran together: perTeam bounds PRs, not repositories", i, repos*perRepo)
		}
	}
	close(release)
	<-done

	require.Equal(t, int64(repos*perRepo), seen.seen.Load(), "every PR runs exactly once")
	require.Equal(t, repos, seen.peakRepos, "perTeam repositories run at once")
	require.Equal(t, perRepo, seen.peakPerRepo, "perRepo PRs of one repository run at once")
}

// TestForEachPR_perRepoOneIsSequential proves the default: the PRs of one
// repository never overlap, which is what avoids "base branch was modified".
func TestForEachPR_perRepoOneIsSequential(t *testing.T) {
	var seen tracker
	groups := groupsOf(3, 4)

	forEachPR(groups, pr.Concurrency{PerTeam: 3, PerRepo: 1}, func(info pr.PRInfo) {
		seen.enter(info.Repo)
		seen.seen.Add(1)
		time.Sleep(10 * time.Millisecond)
		seen.leave(info.Repo)
	})

	require.Equal(t, int64(12), seen.seen.Load())
	require.Equal(t, 1, seen.peakPerRepo, "one PR of a repository at a time")
	require.Equal(t, 3, seen.peakRepos)
}

// TestForEachPR_zeroConcurrencyStillRuns guards the floor: a policy that
// somehow carries a zero bound must not deadlock the sweep.
func TestForEachPR_zeroConcurrencyStillRuns(t *testing.T) {
	var seen tracker
	forEachPR(groupsOf(2, 2), pr.Concurrency{}, func(pr.PRInfo) {
		seen.seen.Add(1)
	})
	require.Equal(t, int64(4), seen.seen.Load())
}
