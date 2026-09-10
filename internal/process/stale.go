package process

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v91/github"
	"github.com/teemow/marge/internal/pr"
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
func (p *Processor) classifyStale(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest, failedChecks []string) *staleResult {
	if len(failedChecks) == 0 {
		return nil
	}
	base := pullReq.GetBase().GetRef()
	head := pullReq.GetHead().GetSHA()
	if base == "" || head == "" {
		return nil
	}

	cmp, _, err := p.Client.Repositories.CompareCommits(ctx, info.Owner, info.Repo, base, head, &github.ListOptions{PerPage: 1})
	if err != nil || cmp.GetBehindBy() <= 0 {
		return nil
	}

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
	p.baseStateMu.Lock()
	if p.baseStateCache == nil {
		p.baseStateCache = make(map[string]map[string]contextState)
	}
	if cached, ok := p.baseStateCache[key]; ok {
		p.baseStateMu.Unlock()
		return cached, nil
	}
	p.baseStateMu.Unlock()

	states := make(map[string]contextState)
	record := func(name string, success, failed bool, at time.Time) {
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
			record(s.GetContext(), state == "success", state == "failure" || state == "error", at)
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
			if cr.GetStatus() != "completed" {
				// Still running on the base branch: neither green nor red.
				record(cr.GetName(), false, false, time.Time{})
				continue
			}
			conclusion := cr.GetConclusion()
			failed := conclusion == "failure" || conclusion == "startup_failure" || conclusion == "timed_out" || conclusion == "cancelled"
			record(cr.GetName(), conclusion == "success", failed, cr.GetCompletedAt().Time)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		checkOpts.Page = resp.NextPage
	}

	p.baseStateMu.Lock()
	p.baseStateCache[key] = states
	p.baseStateMu.Unlock()
	return states, nil
}

// handleStale records the stale classification and, when refreshing is
// enabled and this is not a dry run, updates the PR branch from its base
// (the same merge the "Update branch" button performs) so CI re-runs against
// current code.
//
// The refresh is skipped when the PR carries a non-stale ai-rescue marker,
// including one whose branch was merely rebased since: automation already
// lost on exactly this change, so re-running CI against a newer base cannot
// help, and for a marker without a content fingerprint the refresh would
// even age it out and make the PR look rescuable again. The marker is
// attached to the entry either way so the operator sees it.
func (p *Processor) handleStale(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest, res *staleResult, status *pr.PRStatus, idx int) {
	detail := res.detail()
	status.Update(idx, pr.StatusStale, detail)

	if !p.RefreshStale || p.DryRun {
		return
	}

	if marker := p.findRescueMarker(ctx, info); marker != nil {
		p.markStale(ctx, info, pullReq, marker)
		status.SetRescue(idx, marker)
		if !marker.Stale {
			status.Update(idx, pr.StatusStale, detail+"; refresh skipped: fresh rescue marker")
			return
		}
	}

	_, _, err := p.Client.PullRequests.UpdateBranch(ctx, info.Owner, info.Repo, info.Number, nil)
	// GitHub schedules the update in the background and answers 202, which
	// go-github surfaces as an AcceptedError. That is the success path.
	var accepted *github.AcceptedError
	if err != nil && !errors.As(err, &accepted) {
		status.Update(idx, pr.StatusStale, detail+"; "+ghErrorDetail("refresh failed", err))
		return
	}
	status.Update(idx, pr.StatusRefreshed, "re-checking; "+detail)
}

// staleCache is the per-Processor memo used by baseContextStates. It lives
// in its own struct so Processor literals in tests stay zero-value friendly.
type staleCache struct {
	baseStateMu    sync.Mutex
	baseStateCache map[string]map[string]contextState
}
