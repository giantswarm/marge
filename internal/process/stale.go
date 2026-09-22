package process

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// staleResult describes why a failing PR was classified as stale: its head
// is BehindBy commits behind the base branch, and every failing check named
// in Contexts is green on the base branch head, the newest of them since
// GreenSince.
type staleResult struct {
	Base       string
	BehindBy   int
	Contexts   []string
	GreenSince time.Time
}

// detail renders the stale classification for status output, e.g.
// "go-build green on main since 2026-09-05 10:57 UTC, 12 behind".
func (r *staleResult) detail() string {
	var b strings.Builder
	b.WriteString(strings.Join(r.Contexts, ", "))
	fmt.Fprintf(&b, " green on %s", r.Base)
	if !r.GreenSince.IsZero() {
		fmt.Fprintf(&b, " since %s", r.GreenSince.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if r.BehindBy > 0 {
		fmt.Fprintf(&b, ", %d behind", r.BehindBy)
	}
	return b.String()
}

// contextState is the latest observed outcome of one check name (a commit
// status context or a check-run name) on a commit.
type contextState struct {
	success bool
	failed  bool
	at      time.Time
}

// green reports whether the context can be trusted as passing: at least one
// success was observed and nothing under that name failed. A name that is
// both a passing status and a failing check run is not green.
func (c contextState) green() bool {
	return c.success && !c.failed
}

// recordContext folds one observation of a check name into states: a name
// stays failed once anything under it failed, and keeps its newest time.
func recordContext(states map[string]contextState, name string, success, failed bool, at time.Time) {
	if name == "" {
		return
	}
	st := states[name]
	st.success = st.success || success
	st.failed = st.failed || failed
	if at.After(st.at) {
		st.at = at
	}
	states[name] = st
}

// newestReport is the newest time any context of the head reported. A
// context with no time of its own does not move it.
func newestReport(states map[string]contextState) time.Time {
	var newest time.Time
	for _, st := range states {
		if st.at.After(newest) {
			newest = st.at
		}
	}
	return newest
}

// isFailedConclusion reports whether a completed check run's conclusion
// counts as red.
func isFailedConclusion(conclusion string) bool {
	return conclusion == stateFailure || conclusion == "startup_failure" || conclusion == "timed_out" || conclusion == "cancelled"
}

// classifyStale decides whether a PR's check failure is stale rather than
// real. It returns nil (keep the failed classification) unless all of the
// following hold:
//
//  1. the PR head is behind its base branch (compare base...head, behind_by > 0);
//  2. every failing check has a latest run on the base branch head, and
//     that run is a success.
//
// A context that does not exist on the base branch (a PR-only workflow, a
// job that only runs on pushes) cannot be proven green, so the failure
// stays real. Every API error degrades to "not stale": a lookup problem
// must never soften a failure into a refresh.
func (p *Processor) classifyStale(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest, failedChecks []string, cmp *github.CommitsComparison) *staleResult {
	if len(failedChecks) == 0 || cmp == nil || cmp.GetBehindBy() <= 0 {
		return nil
	}
	base := pullReq.GetBase().GetRef()

	baseSHA := cmp.GetBaseCommit().GetSHA()
	if baseSHA == "" {
		baseSHA = base
	}
	states, err := p.baseContextStates(ctx, info, baseSHA)
	if err != nil {
		return nil
	}

	seen := make(map[string]bool, len(failedChecks))
	res := &staleResult{Base: base, BehindBy: cmp.GetBehindBy()}
	for _, name := range failedChecks {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		st, ok := states[name]
		if !ok || !st.green() {
			return nil
		}
		res.Contexts = append(res.Contexts, name)
		if st.at.After(res.GreenSince) {
			res.GreenSince = st.at
		}
	}
	if len(res.Contexts) == 0 {
		return nil
	}
	sort.Strings(res.Contexts)
	return res
}

// baseContextStates returns the latest state of every commit status context
// and check run on the given base commit. Results are cached per
// owner/repo/sha for the lifetime of the Processor: PRs of one repo share a
// base, so one sweep asks GitHub once per repo instead of once per failing PR.
func (p *Processor) baseContextStates(ctx context.Context, info pr.PRInfo, sha string) (map[string]contextState, error) {
	key := info.Owner + "/" + info.Repo + "@" + sha
	return p.baseStateCache.get(key, func() (map[string]contextState, error) {
		return p.readBaseContextStates(ctx, info, sha)
	})
}

// readBaseContextStates reads what baseContextStates caches.
func (p *Processor) readBaseContextStates(ctx context.Context, info pr.PRInfo, sha string) (map[string]contextState, error) {
	states := make(map[string]contextState)
	record := func(name string, success, failed bool, at time.Time) {
		recordContext(states, name, success, failed, at)
	}

	statusOpts := &github.ListOptions{PerPage: 100}
	for {
		combined, resp, err := p.Client.Repositories.GetCombinedStatus(ctx, info.Owner, info.Repo, sha, statusOpts)
		if err != nil {
			return nil, err
		}
		for _, s := range combined.Statuses {
			state := s.GetState()
			at := s.GetUpdatedAt().Time
			if at.IsZero() {
				at = s.GetCreatedAt().Time
			}
			record(s.GetContext(), state == stateSuccess, state == stateFailure || state == stateError, at)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}

	checkOpts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := p.Client.Checks.ListCheckRunsForRef(ctx, info.Owner, info.Repo, sha, checkOpts)
		if err != nil {
			return nil, err
		}
		for _, cr := range runs.CheckRuns {
			if cr.GetStatus() != statusCompleted {
				// Still running on the base branch: neither green nor red.
				record(cr.GetName(), false, false, time.Time{})
				continue
			}
			conclusion := cr.GetConclusion()
			record(cr.GetName(), conclusion == stateSuccess, isFailedConclusion(conclusion), cr.GetCompletedAt().Time)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		checkOpts.Page = resp.NextPage
	}

	return states, nil
}

// handleStale records the stale classification and, when the refresh
// action is selected and this is not a dry run, updates the PR branch from
// its base (the same merge the "Update branch" button performs) so CI
// re-runs against current code.
//
// The refresh is skipped when the PR carries a non-stale rescue marker,
// including one whose branch was merely rebased since: automation already
// lost on exactly this change, so re-running CI against a newer base cannot
// help, and for a marker without a content fingerprint the refresh would
// even age it out and make the PR look rescuable again. The marker is
// attached to the entry either way so the operator sees it.
func (p *Processor) handleStale(ctx context.Context, run *prRun, res *staleResult) {
	detail := res.detail()
	run.set(pr.StatusStale, detail)

	if !p.Actions.Has(ActionRefresh) || p.DryRun {
		return
	}

	if marker := run.rescueMarker(ctx, p); marker != nil {
		p.markStale(ctx, run.info, run.pull, marker)
		run.status.SetRescue(run.idx, marker)
		if !marker.Stale {
			run.set(pr.StatusStale, detail+"; refresh skipped: fresh rescue marker")
			return
		}
	}

	p.updateBranch(ctx, run, detail)
}

// staleCache is the per-Processor memo used by baseContextStates. It lives
// in its own struct so Processor literals in tests stay zero-value friendly.
type staleCache struct {
	baseStateCache memo[map[string]contextState]
}
