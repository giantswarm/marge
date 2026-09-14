package process

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/google/go-github/v91/github"
)

// cancelledResult describes why a failing PR was classified as cancelled
// rather than failed: every failing check is a CircleCI build that CircleCI
// itself cancelled instead of one that failed a step.
type cancelledResult struct {
	// HeadSHA is the commit whose statuses were read: the PR head as GitHub
	// resolved it for this poll.
	HeadSHA string
	// Builds lists the cancelled builds, one per failing check, in check
	// name order.
	Builds []cancelledBuild
}

// cancelledBuild is one auto-cancelled CircleCI build behind a failing
// check.
type cancelledBuild struct {
	Context  string
	Ref      circleci.BuildRef
	Revision string
}

// onHead reports whether every cancelled build ran on the PR's current
// head. Only then is a retry meaningful: a build behind a newer head has
// been superseded by that head's own build.
func (r *cancelledResult) onHead() bool {
	for _, b := range r.Builds {
		if b.Revision != r.HeadSHA {
			return false
		}
	}
	return len(r.Builds) > 0
}

// detail renders the cancelled classification for status output, e.g.
// "build 1263 auto-cancelled; retry needed" or, behind a newer head,
// "build 1244 (2c0ce64) auto-cancelled; head is now 605a2d6, its build is
// the verdict".
func (r *cancelledResult) detail() string {
	if r.onHead() {
		nums := make([]string, len(r.Builds))
		for i, b := range r.Builds {
			nums[i] = strconv.Itoa(b.Ref.Num)
		}
		label := "build"
		if len(nums) > 1 {
			label = "builds"
		}
		return fmt.Sprintf("%s %s auto-cancelled; retry needed", label, strings.Join(nums, ", "))
	}
	parts := make([]string, len(r.Builds))
	for i, b := range r.Builds {
		parts[i] = fmt.Sprintf("build %d (%s)", b.Ref.Num, shortSHA(b.Revision))
	}
	return fmt.Sprintf("%s auto-cancelled; head is now %s, its build is the verdict",
		strings.Join(parts, ", "), shortSHA(r.HeadSHA))
}

// shortSHA abbreviates a commit SHA the way git does in status output.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// classifyCancelled decides whether a PR's check failure is a CircleCI
// auto-cancel rather than a real failure. It returns nil (keep the failed
// classification) unless every failing check is a commit status whose
// target_url points at a CircleCI build, and every one of those builds was
// cancelled rather than failed (see circleci.Build.AutoCancelled). A failing
// check run, a status from another CI system or a CircleCI build with a
// failed step keeps the failure real.
//
// The lookup is lazy: nothing is fetched unless a failing status points at
// CircleCI, so sweeps without CircleCI failures make no extra calls. When a
// build cannot be inspected (a private project without a token, an API
// error) the failure stays a failure and the returned note tells the
// operator why the build was not classified.
func (p *Processor) classifyCancelled(ctx context.Context, pullReq *github.PullRequest, outcome checkOutcome) (*cancelledResult, string) {
	if p.CircleCI == nil || len(outcome.failedChecks) == 0 {
		return nil, ""
	}
	head := outcome.sha
	if head == "" {
		head = pullReq.GetHead().GetSHA()
	}

	res := &cancelledResult{HeadSHA: head}
	var notes []string
	seen := make(map[string]bool, len(outcome.failedChecks))
	for _, name := range outcome.failedChecks {
		if seen[name] {
			continue
		}
		seen[name] = true
		ref, ok := circleci.ParseBuildURL(outcome.statusTargets[name])
		if !ok {
			// Not a CircleCI build: the failure is real.
			return nil, ""
		}
		build, err := p.CircleCI.Build(ctx, ref)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: build %d could not be inspected (%v)", name, ref.Num, err))
			continue
		}
		if !build.AutoCancelled() {
			return nil, ""
		}
		res.Builds = append(res.Builds, cancelledBuild{Context: name, Ref: ref, Revision: build.VCSRevision})
	}
	if len(notes) > 0 {
		return nil, strings.Join(notes, "; ")
	}
	if len(res.Builds) == 0 {
		return nil, ""
	}
	sort.Slice(res.Builds, func(i, j int) bool { return res.Builds[i].Context < res.Builds[j].Context })
	return res, ""
}

// handleCancelled records the cancelled classification and, when retrying
// is enabled, this is not a dry run and the cancelled builds ran on the
// current head, asks CircleCI to run each of them again on the same commit
// so the PR gets a real verdict. A cancelled build behind a newer head is
// never retried: the new head's own build is the verdict, and the next
// sweep reads it.
func (p *Processor) handleCancelled(ctx context.Context, res *cancelledResult, status *pr.PRStatus, idx int) {
	detail := res.detail()
	status.Update(idx, pr.StatusCancelled, detail)

	if !p.RetryCancelled || p.DryRun || !res.onHead() {
		return
	}
	if !p.CircleCI.HasToken() {
		status.Update(idx, pr.StatusCancelled, detail+"; retry skipped: no CircleCI token configured")
		return
	}

	retried := make([]string, 0, len(res.Builds))
	for _, b := range res.Builds {
		nb, err := p.CircleCI.Retry(ctx, b.Ref)
		if err != nil {
			msg := fmt.Sprintf("retry of build %d failed: %v", b.Ref.Num, err)
			if len(retried) > 0 {
				msg = strings.Join(retried, ", ") + "; " + msg
			}
			status.Update(idx, pr.StatusCancelled, detail+"; "+msg)
			return
		}
		retried = append(retried, fmt.Sprintf("build %d retried as %d", b.Ref.Num, nb.BuildNum))
	}
	status.Update(idx, pr.StatusRetried, "re-checking; "+strings.Join(retried, ", "))
}
