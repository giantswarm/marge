package pr

import (
	"fmt"
	"sort"
	"time"
)

type PRInfo struct {
	Owner     string
	Repo      string
	Number    int
	Title     string
	URL       string
	Author    string
	CreatedAt time.Time
	// BaseRef is the branch the PR targets. Empty when the PR came from a
	// GitHub issue search, which does not report it.
	BaseRef string
}

type PRGroup struct {
	Key   string
	PRs   []PRInfo
	Count int
}

func GroupByRepo(prs []PRInfo) []PRGroup {
	groups := make(map[string][]PRInfo)
	for _, pr := range prs {
		key := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)
		groups[key] = append(groups[key], pr)
	}
	return sortedGroups(groups)
}

func GroupByDependency(prs []PRInfo) []PRGroup {
	groups := make(map[string][]PRInfo)
	for _, pr := range prs {
		dep := ExtractDependencyName(pr.Title)
		if dep == "" {
			dep = "(unknown)"
		}
		groups[dep] = append(groups[dep], pr)
	}
	return sortedGroups(groups)
}

func sortedGroups(groups map[string][]PRInfo) []PRGroup {
	result := make([]PRGroup, 0, len(groups))
	for key, prs := range groups {
		result = append(result, PRGroup{
			Key:   key,
			PRs:   prs,
			Count: len(prs),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})
	return result
}
