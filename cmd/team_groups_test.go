package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/policy"
)

func scopeOf(team string, repos ...string) teamScope {
	return teamScope{team: team, scope: policy.Scope{Repos: repos}}
}

// TestTeamGroups_keepsTeamsThatShareARepositoryTogether: two teams that own
// one repository, directly or through a third team, are one group; the rest
// stand alone, and a team whose scope failed owns nothing.
func TestTeamGroups_keepsTeamsThatShareARepositoryTogether(t *testing.T) {
	scopes := []teamScope{
		scopeOf("atlas", "giantswarm/a", "giantswarm/shared"),
		scopeOf("bumblebee", "giantswarm/b"),
		scopeOf("cabbage", "giantswarm/c", "giantswarm/bridge"),
		scopeOf("honeybadger", "GiantSwarm/Shared ", "giantswarm/bridge"),
		{team: "phoenix", err: errors.New("no team file")},
		scopeOf("rocket"),
	}
	require.Equal(t, [][]int{{0, 2, 3}, {1}, {4}, {5}}, teamGroups(scopes))
}

// TestForEachTeam_sharedRepositoriesNeverRunAtOnce: the teams of one group
// run one after another in the order given while the other groups run at
// the same time.
func TestForEachTeam_sharedRepositoriesNeverRunAtOnce(t *testing.T) {
	scopes := []teamScope{
		scopeOf("atlas", "giantswarm/shared"),
		scopeOf("bumblebee", "giantswarm/b"),
		scopeOf("honeybadger", "giantswarm/shared"),
		scopeOf("phoenix", "giantswarm/p"),
	}
	var (
		mu       sync.Mutex
		running  = map[string]bool{}
		inFlight int
		widest   int
		order    []string
	)
	forEachTeam(scopes, 0, func(index int) {
		team := scopes[index].team
		mu.Lock()
		if team == "atlas" || team == "honeybadger" {
			require.False(t, running["atlas"] || running["honeybadger"], "%s started while its repository was swept", team)
			order = append(order, team)
		}
		running[team] = true
		inFlight++
		widest = max(widest, inFlight)
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		running[team] = false
		inFlight--
		mu.Unlock()
	})
	require.Equal(t, []string{"atlas", "honeybadger"}, order)
	require.Equal(t, 3, widest, "the three groups run at once")

	widest = 0
	forEachTeam(scopes, 1, func(int) {
		mu.Lock()
		inFlight++
		widest = max(widest, inFlight)
		mu.Unlock()
		mu.Lock()
		inFlight--
		mu.Unlock()
	})
	require.Equal(t, 1, widest, "--parallel 1 sweeps one team after another")
}

// TestOrderedOutput_printsTheTeamsInTheOrderNamed: the first team writes
// straight through; a team that finishes before the teams named ahead of it
// is held until they are done, so teams swept at once print one after
// another and the log reads the same however they raced.
func TestOrderedOutput_printsTheTeamsInTheOrderNamed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	out := newOrderedOutput(3, &stdout, &stderr)
	write := func(index int, team string) {
		_, _ = fmt.Fprintf(&out.team(index).stdout, "results %s\n", team)
		_, _ = fmt.Fprintf(&out.team(index).stderr, "team %s\n", team)
	}

	write(2, "phoenix")
	write(1, "bumblebee")
	out.done(2)
	out.done(1)
	require.Empty(t, stderr.String(), "a team behind the first is held")

	write(0, "atlas")
	require.Equal(t, "team atlas\n", stderr.String(), "the first team prints as it goes")

	out.done(0)
	require.Equal(t, "results atlas\nresults bumblebee\nresults phoenix\n", stdout.String())
	require.Equal(t, "team atlas\nteam bumblebee\nteam phoenix\n", stderr.String())
}
