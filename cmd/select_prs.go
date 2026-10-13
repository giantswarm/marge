package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// prRef is one PR a caller named.
type prRef struct {
	owner, repo string
	number      int
}

func (r prRef) key() string { return pr.Key(r.owner, r.repo, r.number) }

// splitByPRs keeps the PRs the caller named, in the order the scope found
// them, and returns the named PRs the scope did not find. Each reference is
// a PR URL or owner/repo#number. An empty selection keeps every PR.
func splitByPRs(prs []pr.PRInfo, refs []string) ([]pr.PRInfo, []prRef, error) {
	if len(refs) == 0 {
		return prs, nil, nil
	}
	selected := make(map[string]prRef, len(refs))
	for _, raw := range refs {
		owner, repo, number, err := pr.ParsePRRef(raw)
		if err != nil {
			return nil, nil, err
		}
		ref := prRef{owner: owner, repo: repo, number: number}
		selected[ref.key()] = ref
	}

	var kept []pr.PRInfo
	for _, info := range prs {
		key := pr.Key(info.Owner, info.Repo, info.Number)
		if _, ok := selected[key]; ok {
			kept = append(kept, info)
			delete(selected, key)
		}
	}
	missing := make([]prRef, 0, len(selected))
	for _, ref := range selected {
		missing = append(missing, ref)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].key() < missing[j].key() })
	return kept, missing, nil
}

// gonePR is a PR the caller named that merged or closed after the caller
// read the list it picked it from.
type gonePR struct {
	info   pr.PRInfo
	merged bool
}

// fate is what became of the PR: merged or closed.
func (g gonePR) fate() string {
	if g.merged {
		return "merged"
	}
	return "closed"
}

// selectPRs keeps the PRs the caller named. A named PR the scope did not
// find is read from GitHub: a bot PR of a repository of the scope that has
// merged or closed is returned as gone, so the run reports it and goes on
// with the rest. Any other is an error, so a caller never believes a PR was
// swept because it was silently absent. An empty repos admits every
// repository.
func selectPRs(ctx context.Context, client *github.Client, prs []pr.PRInfo, refs []string, repos []string) ([]pr.PRInfo, []gonePR, error) {
	kept, missing, err := splitByPRs(prs, refs)
	if err != nil || len(missing) == 0 {
		return kept, nil, err
	}

	inScope := make(map[string]bool, len(repos))
	for _, repo := range repos {
		inScope[strings.ToLower(repo)] = true
	}
	var (
		gone   []gonePR
		absent []string
	)
	for _, ref := range missing {
		if len(inScope) > 0 && !inScope[strings.ToLower(ref.owner+"/"+ref.repo)] {
			absent = append(absent, ref.key())
			continue
		}
		pull, _, err := client.PullRequests.Get(ctx, ref.owner, ref.repo, ref.number)
		if err != nil || pull.GetState() != "closed" || pr.KindOfPull(ref.owner, pull) == "" {
			absent = append(absent, ref.key())
			continue
		}
		gone = append(gone, gonePR{
			info: pr.PRInfo{
				Owner:     ref.owner,
				Repo:      ref.repo,
				Number:    ref.number,
				Title:     pull.GetTitle(),
				URL:       pull.GetHTMLURL(),
				Author:    pull.GetUser().GetLogin(),
				CreatedAt: pull.GetCreatedAt().Time,
				BaseRef:   pull.GetBase().GetRef(),
				HeadRef:   pull.GetHead().GetRef(),
			},
			merged: pull.GetMerged(),
		})
	}
	if len(absent) > 0 {
		return nil, nil, fmt.Errorf("not an open bot PR of this scope: %s", strings.Join(absent, ", "))
	}
	return kept, gone, nil
}

// reportGone adds each gone PR to the run's status as what became of it.
func reportGone(status *pr.PRStatus, gone []gonePR) {
	for _, g := range gone {
		idx := status.Add(g.info)
		status.SetClassification(idx, pr.DiscoveredKind(g.info), "")
		state := pr.StatusSkipped
		if g.merged {
			state = pr.StatusAlreadyMerged
		}
		status.Update(idx, state, g.fate()+" since the list was read")
	}
}
