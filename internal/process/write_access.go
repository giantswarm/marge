package process

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// errNoWriteAccess refuses an approval the reviewer's access would not let
// count. GitHub evaluates the reviewer's repository write access when it
// decides whether a review satisfies a required-approvals rule: without it
// the review is created and visible, but stays out of
// latestOpinionatedReviews and reviewDecision stays REVIEW_REQUIRED, with no
// error anywhere. For a GitHub App installation, contents: write is the
// permission that confers that access.
var errNoWriteAccess = errors.New("no write access to the repository")

// ensureWriteAccess returns nil when the authenticated actor holds write
// access to the repository, and errNoWriteAccess when it does not. The answer
// is read once per repository and memoised for the rest of the sweep.
func (p *Processor) ensureWriteAccess(ctx context.Context, owner, repo string) error {
	key := owner + "/" + repo

	p.writeAccessMu.Lock()
	if p.writeAccessCache == nil {
		p.writeAccessCache = make(map[string]bool)
	}
	allowed, cached := p.writeAccessCache[key]
	p.writeAccessMu.Unlock()

	if !cached {
		repository, _, err := p.Client.Repositories.Get(ctx, owner, repo)
		if err != nil {
			return fmt.Errorf("write access check on %s: %w", key, err)
		}
		allowed = repository.GetPermissions().GetPush()

		p.writeAccessMu.Lock()
		p.writeAccessCache[key] = allowed
		p.writeAccessMu.Unlock()
	}

	if !allowed {
		return fmt.Errorf("%w: %s; a GitHub App needs contents: write for its approval to count", errNoWriteAccess, key)
	}
	return nil
}

// accessCache is the per-Processor memo used by ensureWriteAccess. It lives
// in its own struct so Processor literals in tests stay zero-value friendly.
type accessCache struct {
	writeAccessMu    sync.Mutex
	writeAccessCache map[string]bool
}
