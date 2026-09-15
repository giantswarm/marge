package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/marge/internal/pr"
)

// File is one policy file and the path it was read from. The path travels
// with the document so a resolved policy can name the files that made it.
type File struct {
	Path string
	Doc  *Document
}

// Set is the policy of one sweep: the policy every repository of the scope
// is swept under, and the repositories whose own exception deviates from
// it. A Set is read-only once built, so the sweep's goroutines share one.
type Set struct {
	base   pr.Policy
	byRepo map[string]pr.Policy
}

// NewSet resolves the base policy from the files, applies each repository
// exception on top of it, and returns the result. Exceptions are keyed by
// repository name, matched the way GitHub matches them, without case.
func NewSet(files []File, exceptions map[string]Exception) (*Set, error) {
	base := pr.CompanyDefaults()
	for _, file := range files {
		if file.Doc == nil {
			continue
		}
		if !file.Doc.resolved {
			return nil, fmt.Errorf("policy file %s: only ParseDocument builds a document the sweep can apply", file.Path)
		}
		base = apply(base, file.Path, file.Doc)
	}
	set := &Set{base: base}
	if len(exceptions) == 0 {
		return set, nil
	}
	set.byRepo = make(map[string]pr.Policy, len(exceptions))
	for repo, exception := range exceptions {
		resolved, err := exception.apply(base, repo)
		if err != nil {
			return nil, err
		}
		set.byRepo[exceptionKey(repo)] = resolved
	}
	return set, nil
}

// For returns the policy repo is swept under. repo is a repository name,
// with or without its owner: "marge" and "giantswarm/marge" resolve to the
// same policy, because a team's exceptions cover one owner only. The
// returned policy is read-only: callers never write to it or to its map.
func (s *Set) For(repo string) pr.Policy {
	if s == nil {
		return pr.CompanyDefaults()
	}
	if resolved, ok := s.byRepo[exceptionKey(repo)]; ok {
		return resolved
	}
	return s.base
}

// exceptionKey is how a repository name keys the exception map: without its
// owner, and without case, the way GitHub matches a repository name.
func exceptionKey(repo string) string {
	if _, name, found := strings.Cut(repo, "/"); found {
		repo = name
	}
	return strings.ToLower(repo)
}

// Base returns the policy of a repository without an exception. The sweep
// reads its concurrency and its Slack channel from it.
func (s *Set) Base() pr.Policy {
	if s == nil {
		return pr.CompanyDefaults()
	}
	return s.base
}

// apply layers one document on top of a policy. Only the fields the
// document sets change, and the document's path joins the sources.
func apply(base pr.Policy, path string, doc *Document) pr.Policy {
	out := base.Clone()
	out.Sources = append(out.Sources, path)

	for kind, types := range doc.updateTypes {
		out.UpdateTypes[kind] = slices.Clone(types)
	}
	if doc.Schedule != nil {
		out.Schedule = *doc.Schedule == scheduleEnabled
	}
	if r := doc.Rescue; r != nil {
		if r.Enabled != nil {
			out.Rescue.Enabled = *r.Enabled
		}
		if r.Timeout != nil {
			out.Rescue.Timeout = r.timeout
		}
		if r.Weekly != nil {
			out.Rescue.Weekly = *r.Weekly
		}
		if b := r.Budget; b != nil {
			if b.PerRescue != nil {
				out.Rescue.Budget.PerRescueUSD = *b.PerRescue
			}
			if b.Weekly != nil {
				out.Rescue.Budget.WeeklyUSD = *b.Weekly
			}
		}
		if r.Confirm != nil {
			out.Rescue.Confirm = pr.Confirm(*r.Confirm)
		}
	}
	if c := doc.Concurrency; c != nil {
		if c.PerTeam != nil {
			out.Concurrency.PerTeam = *c.PerTeam
		}
		if c.PerRepo != nil {
			out.Concurrency.PerRepo = *c.PerRepo
		}
	}
	if doc.ModelConfig != nil {
		out.ModelConfig = *doc.ModelConfig
	}
	if doc.SlackChannel != nil {
		out.SlackChannel = *doc.SlackChannel
	}
	return out
}

// apply layers a repository exception on top of the team policy. Every key
// of an exception narrows: an update type list is intersected with the
// team's, never added to, and a rescue the team switched off stays off. A
// key that tries to widen is an error, not a silent narrowing, so a team
// reads back what it wrote. The list reaches only the kinds that name a
// version. The sweep itself has no team-level switch, so enabled sets it
// either way.
func (e Exception) apply(base pr.Policy, repo string) (pr.Policy, error) {
	allowed, err := e.resolve(repo)
	if err != nil {
		return pr.Policy{}, err
	}
	out := base.Clone()
	out.Sources = append(out.Sources, fmt.Sprintf("botPRsSweep of repository %s", repo))

	if e.Enabled != nil {
		out.Sweep = *e.Enabled
	}
	if e.Rescue != nil {
		if *e.Rescue && !base.Rescue.Enabled {
			return pr.Policy{}, fmt.Errorf("botPRsSweep.rescue of repository %s: an exception cannot switch the rescues on where the team switched them off", repo)
		}
		out.Rescue.Enabled = *e.Rescue
	}
	if allowed != nil {
		for _, updateType := range allowed {
			if !base.AllowsUpdate(updateType) {
				return pr.Policy{}, fmt.Errorf("botPRsSweep.updateTypes of repository %s: the team merges no %s update, and an exception cannot add one", repo, updateType)
			}
		}
		// Align files and Herald PRs name no version, so an exception that
		// restricts update types leaves those two kinds alone.
		for kind, types := range out.UpdateTypes {
			if !kind.CarriesVersion() {
				continue
			}
			kept := make([]pr.UpdateType, 0, len(types))
			for _, updateType := range types {
				if slices.Contains(allowed, updateType) {
					kept = append(kept, updateType)
				}
			}
			out.UpdateTypes[kind] = kept
		}
	}
	return out, nil
}
