package cmd

import (
	"strings"

	"github.com/giantswarm/marge/internal/fanout"
)

// forEachTeam calls fn for every team of scopes, parallel teams at once as
// fanout.Each reads it. Teams that own a common repository, directly or
// through a third team, form one group whose teams run one after another in
// the order given: a sweep writes, and two sweeps must never act on one PR
// at the same time. A team whose scope did not resolve owns nothing and
// shares no group.
func forEachTeam(scopes []teamScope, parallel int, fn func(index int)) {
	groups := teamGroups(scopes)
	fanout.Each(len(groups), parallel, func(g int) {
		for _, index := range groups[g] {
			fn(index)
		}
	})
}

// teamGroups partitions the teams into groups that share no repository. The
// groups are ordered by their first team and each lists its teams in the
// order given, so the same scopes always group the same way.
func teamGroups(scopes []teamScope) [][]int {
	parent := make([]int, len(scopes))
	for i := range parent {
		parent[i] = i
	}
	root := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}

	owner := make(map[string]int)
	for index, scoped := range scopes {
		if scoped.err != nil {
			continue
		}
		for _, repo := range scoped.scope.Repos {
			key := strings.ToLower(strings.TrimSpace(repo))
			first, owned := owner[key]
			if !owned {
				owner[key] = index
				continue
			}
			// The lower index becomes the root, so a group is named by its
			// first team.
			a, b := root(first), root(index)
			if a != b {
				parent[max(a, b)] = min(a, b)
			}
		}
	}

	var groups [][]int
	groupOf := make(map[int]int)
	for index := range scopes {
		r := root(index)
		g, seen := groupOf[r]
		if !seen {
			g = len(groups)
			groupOf[r] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], index)
	}
	return groups
}
