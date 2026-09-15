package process

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// protection is what the sweep needs from a base branch's protection: the
// required status check contexts and whether the branch must be up to date
// before merging.
type protection struct {
	Contexts []string
	Strict   bool
	// Readable is false when GitHub refused to show the protection. Only a
	// repository admin may read it, and only an admin may merge past it
	// when enforce_admins is off, so a caller who cannot read it cannot
	// bypass it either: GitHub enforces the checks for that caller.
	Readable bool
}

// protectionCache memoises requiredProtection per owner/repo@base for the
// lifetime of the Processor: every PR of a repository shares one base.
type protectionCache struct {
	protectionMu sync.Mutex
	protections  map[string]protection
}

// requiredProtection reads the base branch's required status checks. A 404
// means the branch has no protection or no required checks: nothing is
// required. A 403 means the caller is not an admin: the protection stays
// unknown and GitHub itself enforces it for that caller. Any other error is
// returned so a transient failure never softens into "nothing required".
func (p *Processor) requiredProtection(ctx context.Context, info pr.PRInfo, base string) (protection, error) {
	key := info.Owner + "/" + info.Repo + "@" + base
	p.protectionMu.Lock()
	if p.protections == nil {
		p.protections = make(map[string]protection)
	}
	if cached, ok := p.protections[key]; ok {
		p.protectionMu.Unlock()
		return cached, nil
	}
	p.protectionMu.Unlock()

	var result protection
	checks, resp, err := p.Client.Repositories.GetRequiredStatusChecks(ctx, info.Owner, info.Repo, base)
	switch {
	case err == nil:
		result = protection{Readable: true, Strict: checks.Strict}
		for _, c := range checks.GetChecks() {
			if c.Context != "" {
				result.Contexts = append(result.Contexts, c.Context)
			}
		}
		if len(result.Contexts) == 0 {
			result.Contexts = append(result.Contexts, checks.GetContexts()...)
		}
	case isStatus(err, resp, http.StatusNotFound):
		result = protection{Readable: true}
	case isStatus(err, resp, http.StatusForbidden):
		result = protection{Readable: false}
	default:
		return protection{}, err
	}

	p.protectionMu.Lock()
	p.protections[key] = result
	p.protectionMu.Unlock()
	return result, nil
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
