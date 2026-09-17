// Package patterns reads back the unhandled-failure markers the sweep
// writes on pull requests, and groups them by signature.
//
// A sweep run groups the failures no rule recognised in memory and prints
// the groups; the run then ends and the grouping ends with it. The markers
// stay on the pull requests, so they are what a later command reads: the
// skeleton `marge rules draft` builds, and the count that says whether a
// pattern is seen often enough to be worth a rule.
package patterns

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// Group is one shape of failure, with the pull requests that carry it.
type Group struct {
	Signature string
	Checks    []string
	// Excerpt is the log the first marker of the group recorded. Every
	// marker of one signature describes the same failure, so one excerpt
	// is enough to build a rule against.
	Excerpt string
	Count   int
	PRs     []string
	// LastSeen is the newest marker of the group.
	LastSeen time.Time
}

// Search finds the markers over one organization.
type Search struct {
	Client *github.Client
	Org    string
	// Since bounds the search to pull requests updated on or after it, and
	// drops markers written before it. A zero time reads the whole history
	// the search returns.
	Since time.Time
}

// searchPageSize is what one search page holds, and commentPageSize what one
// comment page holds. Both are GitHub's maximum, so a pattern on fifty pull
// requests costs as few requests as it can.
const (
	searchPageSize  = 100
	commentPageSize = 100
)

// Signature returns the group of one signature, or nil when no pull request
// carries it.
func (s Search) Signature(ctx context.Context, signature string) (*Group, error) {
	groups, err := s.groups(ctx, fmt.Sprintf("%q", signature), signature)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	return &groups[0], nil
}

// Top returns every signature the search finds, the one on the most pull
// requests first.
func (s Search) Top(ctx context.Context) ([]Group, error) {
	return s.groups(ctx, fmt.Sprintf("%q", pr.UnhandledPhrase), "")
}

// groups runs one search and collects the markers of the pull requests it
// returns. term is what the comment must contain; signature, when set,
// keeps only the markers of that one signature.
func (s Search) groups(ctx context.Context, term, signature string) ([]Group, error) {
	if s.Org == "" {
		return nil, fmt.Errorf("no organization to search")
	}

	query := fmt.Sprintf("org:%s is:pr in:comments %s", s.Org, term)
	if !s.Since.IsZero() {
		query += " updated:>=" + s.Since.UTC().Format(time.DateOnly)
	}

	opts := &github.SearchOptions{Sort: "updated", ListOptions: github.ListOptions{PerPage: searchPageSize}}
	bySignature := make(map[string]*Group)
	var order []string
	for {
		result, resp, err := s.Client.Search.Issues(ctx, query, opts)
		if err != nil {
			return nil, fmt.Errorf("searching %s for unhandled markers: %w", s.Org, err)
		}
		for _, issue := range result.Issues {
			owner, repo, err := pr.ExtractOwnerRepo(issue.GetHTMLURL())
			if err != nil {
				continue
			}
			marker, err := s.newestMarker(ctx, owner, repo, issue.GetNumber(), signature)
			if err != nil {
				return nil, err
			}
			if marker == nil || (!s.Since.IsZero() && marker.At.Before(s.Since)) {
				continue
			}
			key := marker.Signature
			group, seen := bySignature[key]
			if !seen {
				group = &Group{Signature: key, Checks: marker.Checks, Excerpt: marker.Excerpt}
				bySignature[key] = group
				order = append(order, key)
			}
			group.Count++
			group.PRs = append(group.PRs, fmt.Sprintf("%s/%s#%d", owner, repo, issue.GetNumber()))
			if marker.At.After(group.LastSeen) {
				group.LastSeen = marker.At
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	out := make([]Group, 0, len(order))
	for _, key := range order {
		out = append(out, *bySignature[key])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

// newestMarker returns the last unhandled marker of one pull request, or
// nil when it carries none. With a signature it returns the last marker of
// that signature, so a pull request that failed two ways over its life
// counts under the one asked for.
func (s Search) newestMarker(ctx context.Context, owner, repo string, number int, signature string) (*pr.RescueMarker, error) {
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: commentPageSize}}
	var newest *pr.RescueMarker
	for {
		comments, resp, err := s.Client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("reading the comments of %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, comment := range comments {
			marker := pr.ParseRescueMarker(comment.GetBody())
			if marker == nil || !marker.IsUnhandled() {
				continue
			}
			if signature != "" && marker.Signature != signature {
				continue
			}
			newest = marker
		}
		if resp == nil || resp.NextPage == 0 {
			return newest, nil
		}
		opts.Page = resp.NextPage
	}
}

// Known renders the signatures a search found, for the message that says a
// wanted one is not among them.
func Known(groups []Group) string {
	lines := make([]string, 0, len(groups))
	for _, group := range groups {
		lines = append(lines, fmt.Sprintf("%s (%d PRs, %s)", group.Signature, group.Count, strings.Join(group.Checks, ", ")))
	}
	return strings.Join(lines, "\n  ")
}
