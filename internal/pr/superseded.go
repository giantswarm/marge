package pr

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Renovate leaves two open PRs for one dependency whenever a major lands
// while a minor is still open: #29 moves actions/checkout to v5.1.0 and #30
// moves it to v7.0.1. Only the higher one is worth merging, but both are
// reported independently, so the lower one looks like work -- and when it
// also carries a merge conflict it looks like work for a rescue agent.

// SupersededBy maps a PR to the detail line naming the sibling that
// supersedes it, keyed by PRKey. It is computed once per sweep from the PR
// list and read without locking while the PRs are processed.
type SupersededBy map[string]string

// PRKey identifies one PR across repositories.
func PRKey(p PRInfo) string {
	return fmt.Sprintf("%s/%s#%d", p.Owner, p.Repo, p.Number)
}

// FindSuperseded returns, for every PR that a sibling supersedes, the detail
// naming the PR that supersedes it. Two PRs are siblings when they are open
// on the same repository and the same base branch and their titles name the
// same dependency; the one whose target version is highest wins and every
// other one is superseded.
//
// A PR is only ever superseded on positive evidence: both titles must name
// a dependency and a target version, and both versions must parse as
// semver. Equal versions supersede nothing, because neither is higher.
//
// PRInfo.BaseRef is empty on the PRs that come from a GitHub issue search,
// which does not report a base branch. Those group together as before, so a
// repository with a maintenance branch can group two PRs that target
// different branches. A supersession never suppresses a merge, so the cost
// of such a group is a mislabelled report line.
func FindSuperseded(prs []PRInfo) SupersededBy {
	type candidate struct {
		info    PRInfo
		version *semver.Version
		raw     string
	}

	groups := make(map[string][]candidate)
	for _, p := range prs {
		dep := strings.ToLower(ExtractDependencyName(p.Title))
		if dep == "" {
			continue
		}
		raw := ExtractTargetVersion(p.Title)
		if raw == "" {
			continue
		}
		version, err := semver.NewVersion(raw)
		if err != nil {
			continue
		}
		key := fmt.Sprintf("%s/%s\x00%s\x00%s", p.Owner, p.Repo, p.BaseRef, dep)
		groups[key] = append(groups[key], candidate{info: p, version: version, raw: raw})
	}

	out := make(SupersededBy)
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		best := group[0]
		for _, c := range group[1:] {
			if c.version.GreaterThan(best.version) {
				best = c
			}
		}
		for _, c := range group {
			if !best.version.GreaterThan(c.version) {
				continue
			}
			out[PRKey(c.info)] = fmt.Sprintf("superseded by #%d (%s)", best.info.Number, best.raw)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
