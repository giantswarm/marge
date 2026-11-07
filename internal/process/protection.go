package process

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-github/v93/github"

	"github.com/giantswarm/marge/internal/pr"
)

// protection is what the sweep needs from a base branch's protection: the
// required status check contexts, and whether the branch must be up to date
// before a merge.
type protection struct {
	Contexts []string
	// Strict is "require branches to be up to date before merging". Under
	// it every merge puts the sibling PRs of the repository behind their
	// base, so the repository merges one PR per sweep.
	Strict bool
}

// reviewRule is what the sweep needs from a base branch's review
// enforcement.
type reviewRule struct {
	// CodeOwnerReviews is require_code_owner_reviews: an approval counts
	// only when a code owner gives it. The sweep App owns no code, so its
	// own approval never satisfies the rule and GitHub refuses the merge.
	CodeOwnerReviews bool
}

// protectionCache memoises requiredProtection, requiredReview and the
// branch's rulesets per owner/repo@base for the lifetime of the Processor:
// every PR of a repository shares one base.
type protectionCache struct {
	protections memo[protection]
	reviews     memo[reviewRule]
	rules       memo[*github.BranchRules]
	classic     classicAccess
}

// classicAccess is whether the sweep's token may read classic branch
// protection at all. A token without the administration permission is
// refused on every repository alike ("Resource not accessible by
// integration"), so the sweep's first read answers for every later one, and
// the later ones are not sent. A refusal for want of the admin role is the
// repository's own and answers for nothing else.
type classicAccess struct {
	mu     sync.Mutex
	known  bool
	denied bool
}

// readClassic sends read, a read of a base branch's classic protection,
// unless the token may not read classic protection, and reports whether it
// was sent. Until one answer settles the question the reads go one at a
// time, so the repositories a sweep starts together do not each spend a
// read on the refusal the first one meets.
func (p *Processor) readClassic(read func() (*github.Response, error)) (bool, *github.Response, error) {
	c := &p.classic
	c.mu.Lock()
	if c.known {
		denied := c.denied
		c.mu.Unlock()
		if denied {
			return false, nil, nil
		}
		resp, err := read()
		return true, resp, err
	}
	defer c.mu.Unlock()
	resp, err := read()
	switch {
	case err == nil, isStatus(err, resp, http.StatusNotFound):
		c.known = true
	case isStatus(err, resp, http.StatusForbidden) && !isRateLimit(err):
		c.known, c.denied = true, isTokenRefusal(err)
	}
	return true, resp, err
}

// isTokenRefusal reports whether GitHub refused the call for what the
// token may do, not for what its holder may do on this repository.
func isTokenRefusal(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && strings.HasPrefix(ghErr.Message, "Resource not accessible by")
}

// branchRules reads the rulesets that apply to the base branch. A branch can
// carry classic protection, rulesets or both, and GitHub enforces all of
// them, so every reader of the base branch's rules reads both. A 404 or 403
// means no ruleset the caller may see; a rate refusal or any other error is
// returned, for the reasons requiredProtection gives.
func (p *Processor) branchRules(ctx context.Context, info pr.PRInfo, base string) (*github.BranchRules, error) {
	key := info.Owner + "/" + info.Repo + "@" + base
	return p.rules.get(key, func() (*github.BranchRules, error) {
		rules, resp, err := p.Client.Repositories.ListRulesForBranch(ctx, info.Owner, info.Repo, base, nil)
		switch {
		case err == nil:
			if rules == nil {
				rules = &github.BranchRules{}
			}
			return rules, nil
		case isRateLimit(err):
			return nil, err
		case isStatus(err, resp, http.StatusNotFound), isStatus(err, resp, http.StatusForbidden):
			return &github.BranchRules{}, nil
		default:
			return nil, err
		}
	})
}

// requiredProtection reads the base branch's required status checks, from
// its classic protection and from its rulesets together: a repository set
// up by devctl carries only the ruleset. A 404
// means the branch has no protection or no required checks: nothing is
// required. A 403 means the caller may not read it (see readClassic for a
// token that may not read it anywhere): only an admin may merge
// past the protection when enforce_admins is off, so GitHub enforces the
// checks for that caller and the merge refusal is classified as a wait. Any
// other error is returned so a transient failure never softens into
// "nothing required".
func (p *Processor) requiredProtection(ctx context.Context, info pr.PRInfo, base string) (protection, error) {
	key := info.Owner + "/" + info.Repo + "@" + base
	return p.protections.get(key, func() (protection, error) {
		var result protection
		var checks *github.RequiredStatusChecks
		sent, resp, err := p.readClassic(func() (*github.Response, error) {
			var resp *github.Response
			var err error
			checks, resp, err = p.Client.Repositories.GetRequiredStatusChecks(ctx, info.Owner, info.Repo, base)
			return resp, err
		})
		switch {
		case !sent:
		case err == nil:
			result.Strict = checks.GetStrict()
			for _, c := range checks.GetChecks() {
				if c.Context != "" {
					result.Contexts = append(result.Contexts, c.Context)
				}
			}
			if len(result.Contexts) == 0 {
				result.Contexts = append(result.Contexts, checks.GetContexts()...)
			}
		case isRateLimit(err):
			return protection{}, err
		case isStatus(err, resp, http.StatusNotFound), isStatus(err, resp, http.StatusForbidden):
		default:
			return protection{}, err
		}
		rules, err := p.branchRules(ctx, info, base)
		if err != nil {
			return protection{}, err
		}
		for _, rule := range rules.RequiredStatusChecks {
			result.Strict = result.Strict || rule.Parameters.StrictRequiredStatusChecksPolicy
			for _, c := range rule.Parameters.RequiredStatusChecks {
				if c != nil && c.Context != "" && !slices.Contains(result.Contexts, c.Context) {
					result.Contexts = append(result.Contexts, c.Context)
				}
			}
		}
		return result, nil
	})
}

// requiredReview reads the base branch's review enforcement, from its classic
// protection and from its rulesets' pull request rules. It handles the
// two statuses requiredProtection handles, for the same reasons: a 404 means
// the branch enforces no review, and a 403 means the caller may not read the
// protection, which is not a rule the sweep may claim.
func (p *Processor) requiredReview(ctx context.Context, info pr.PRInfo, base string) (reviewRule, error) {
	key := info.Owner + "/" + info.Repo + "@" + base
	return p.reviews.get(key, func() (reviewRule, error) {
		var result reviewRule
		var enforcement *github.PullRequestReviewsEnforcement
		sent, resp, err := p.readClassic(func() (*github.Response, error) {
			var resp *github.Response
			var err error
			enforcement, resp, err = p.Client.Repositories.GetPullRequestReviewEnforcement(ctx, info.Owner, info.Repo, base)
			return resp, err
		})
		switch {
		case !sent:
		case err == nil:
			result.CodeOwnerReviews = enforcement.GetRequireCodeOwnerReviews()
		case isRateLimit(err):
			return reviewRule{}, err
		case isStatus(err, resp, http.StatusNotFound), isStatus(err, resp, http.StatusForbidden):
		default:
			return reviewRule{}, err
		}
		rules, err := p.branchRules(ctx, info, base)
		if err != nil {
			return reviewRule{}, err
		}
		for _, rule := range rules.PullRequest {
			result.CodeOwnerReviews = result.CodeOwnerReviews || rule.Parameters.RequireCodeOwnerReview
		}
		return result, nil
	})
}

// isRateLimit reports whether the error is GitHub refusing the call for rate
// rather than for access. Both arrive as 403, and the answer to a rate
// refusal is to ask again later, never to read it as "the branch requires
// nothing": the memo would keep that answer for the rest of the sweep.
func isRateLimit(err error) bool {
	var primary *github.RateLimitError
	var secondary *github.AbuseRateLimitError
	return errors.As(err, &primary) || errors.As(err, &secondary)
}

func isStatus(err error, resp *github.Response, code int) bool {
	if resp != nil && resp.StatusCode == code {
		return true
	}
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == code
}

// requiredOutcome partitions the required contexts by what the PR head
// reported for them. A context nobody reported is missing, and missing is
// a wait: it may report later or never, and neither case is a pass.
type requiredOutcome struct {
	Green   []string
	Pending []string
	Failed  []string
	Missing []string
}

func (r requiredOutcome) allGreen() bool {
	return len(r.Pending) == 0 && len(r.Failed) == 0 && len(r.Missing) == 0
}

// evaluateRequired matches every required context against the states the
// PR head reported. GitHub Actions jobs report under their job name while a
// protection may list them as "<workflow> / <job>", so a required context
// also matches a reported name that is its last " / " segment, and the
// other way round.
func evaluateRequired(required []string, reported map[string]contextState) requiredOutcome {
	var out requiredOutcome
	for _, ctxName := range required {
		st, ok := lookupContext(reported, ctxName)
		switch {
		case !ok:
			out.Missing = append(out.Missing, ctxName)
		case st.failed:
			out.Failed = append(out.Failed, ctxName)
		case st.success:
			out.Green = append(out.Green, ctxName)
		default:
			out.Pending = append(out.Pending, ctxName)
		}
	}
	return out
}

func lookupContext(reported map[string]contextState, name string) (contextState, bool) {
	if st, ok := reported[name]; ok {
		return st, true
	}
	short := lastSegment(name)
	if short != name {
		if st, ok := reported[short]; ok {
			return st, true
		}
	}
	for reportedName, st := range reported {
		if lastSegment(reportedName) == name {
			return st, true
		}
	}
	return contextState{}, false
}

func lastSegment(name string) string {
	if i := strings.LastIndex(name, " / "); i >= 0 {
		return name[i+3:]
	}
	return name
}

// isRequired reports whether a reported check name is one of the required
// contexts, with the same name matching as evaluateRequired.
func isRequired(required []string, name string) bool {
	for _, ctxName := range required {
		if ctxName == name || lastSegment(ctxName) == name || lastSegment(name) == ctxName {
			return true
		}
	}
	return false
}
