package policy

import (
	"fmt"
	"slices"
	"strings"
	"time"

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
	base     pr.Policy
	byRepo   map[string]pr.Policy
	fileList []string
}

// NewSet resolves the base policy from the files, applies each repository
// exception on top of it, and returns the result. Exceptions are keyed by
// repository name, matched the way GitHub matches them, without case.
func NewSet(files []File, exceptions map[string]Exception) (*Set, error) {
	base := pr.CompanyDefaults()
	var paths []string
	for _, file := range files {
		if file.Doc == nil {
			continue
		}
		base = apply(base, file.Path, file.Doc)
		paths = append(paths, file.Path)
	}
	set := &Set{base: base, fileList: paths}
	if len(exceptions) == 0 {
		return set, nil
	}
	set.byRepo = make(map[string]pr.Policy, len(exceptions))
	for repo, exception := range exceptions {
		resolved, err := exception.apply(base, repo)
		if err != nil {
			return nil, err
		}
		set.byRepo[strings.ToLower(repo)] = resolved
	}
	return set, nil
}

// For returns the policy repo is swept under. The returned policy is
// read-only: callers never write to it or to its map.
func (s *Set) For(repo string) pr.Policy {
	if s == nil {
		return pr.CompanyDefaults()
	}
	if resolved, ok := s.byRepo[strings.ToLower(repo)]; ok {
		return resolved
	}
	return s.base
}

// Base returns the policy of a repository without an exception. The sweep
// reads its concurrency and its Slack channel from it.
func (s *Set) Base() pr.Policy {
	if s == nil {
		return pr.CompanyDefaults()
	}
	return s.base
}

// Files names the policy files the set was built from, in the order they
// were applied. It is empty when no file was found and the built-in
// defaults apply on their own.
func (s *Set) Files() []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.fileList)
}

// apply layers one document on top of a policy. Only the fields the
// document sets change, and the document's path joins the sources.
func apply(base pr.Policy, path string, doc *Document) pr.Policy {
	out := base.Clone()
	out.Sources = append(out.Sources, path)

	for kind, names := range doc.UpdateTypes {
		// Both maps were validated at parse time, so neither lookup can
		// fail here.
		types, _ := updateTypes(names)
		out.UpdateTypes[knownKinds[kind]] = types
	}
	if doc.Schedule != nil {
		out.Schedule = *doc.Schedule == scheduleEnabled
	}
	if r := doc.Rescue; r != nil {
		if r.Enabled != nil {
			out.Rescue.Enabled = *r.Enabled
		}
		if r.Timeout != nil {
			timeout, _ := time.ParseDuration(*r.Timeout)
			out.Rescue.Timeout = timeout
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
// of an exception narrows: the sweep and the rescues can only be switched
// off, and an update type list is intersected with the team's, never added
// to. A key that tries to widen is an error, not a silent narrowing, so a
// team reads back what it wrote.
func (e Exception) apply(base pr.Policy, repo string) (pr.Policy, error) {
	if err := e.validate(repo); err != nil {
		return pr.Policy{}, err
	}
	out := base.Clone()
	out.Sources = append(out.Sources, fmt.Sprintf("botPRsSweep of repository %s", repo))

	if e.Enabled != nil {
		if *e.Enabled && !base.Sweep {
			return pr.Policy{}, fmt.Errorf("botPRsSweep.enabled of repository %s: an exception cannot switch the sweep on where the team switched it off", repo)
		}
		out.Sweep = *e.Enabled
	}
	if e.Rescue != nil {
		if *e.Rescue && !base.Rescue.Enabled {
			return pr.Policy{}, fmt.Errorf("botPRsSweep.rescue of repository %s: an exception cannot switch the rescues on where the team switched them off", repo)
		}
		out.Rescue.Enabled = *e.Rescue
	}
	if e.UpdateTypes != nil {
		allowed, _ := updateTypes(e.UpdateTypes)
		for kind, types := range out.UpdateTypes {
			kept := make([]pr.UpdateType, 0, len(types))
			for _, t := range types {
				if slices.Contains(allowed, t) {
					kept = append(kept, t)
				}
			}
			out.UpdateTypes[kind] = kept
		}
	}
	return out, nil
}
