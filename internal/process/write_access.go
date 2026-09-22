package process

import (
	"context"
	"errors"
	"fmt"
)

// errNoWriteAccess refuses an approval the reviewer's access would not let
// count. GitHub evaluates the reviewer's repository write access when it
// decides whether a review satisfies a required-approvals rule: without it
// the review is created and visible, but stays out of
// latestOpinionatedReviews and reviewDecision stays REVIEW_REQUIRED, with no
// error anywhere. For a GitHub App installation, contents: write is the
// permission that confers that access.
var errNoWriteAccess = errors.New("no write access to the repository")

// errWriteAccessUnknown separates "GitHub reported no write access" from
// "GitHub reported nothing to read". The repository payload carries
// permissions.push only for an actor GitHub resolves permissions for; an
// absent field is an unanswered question, not a denial. Both refuse the
// approval, because an approval that does not count is worse than a stopped
// sweep, but only one of them means the permission set is wrong.
var errWriteAccessUnknown = errors.New("repository carried no permissions.push field")

// AppWriteAccess reports whether the GitHub App installation the sweep
// authenticates as may write to the repository. A nil AppWriteAccess is a
// sweep running under a person's token, where permissions.push on the
// repository is the answer instead.
type AppWriteAccess func(ctx context.Context, owner, repo string) (bool, error)

// ensureWriteAccess returns nil when the authenticated actor holds write
// access to the repository, errNoWriteAccess when GitHub reports it does not,
// and errWriteAccessUnknown when GitHub reports no answer at all. A settled
// answer is read once per repository and memoised for the rest of the sweep.
func (p *Processor) ensureWriteAccess(ctx context.Context, owner, repo string) error {
	key := owner + "/" + repo

	allowed, err := p.writeAccess.get(key, func() (bool, error) {
		return p.readWriteAccess(ctx, owner, repo)
	})
	if err != nil {
		return err
	}

	if !allowed {
		return fmt.Errorf("%w: %s; a GitHub App needs contents: write for its approval to count", errNoWriteAccess, key)
	}
	return nil
}

// readWriteAccess asks the question the authenticated actor can answer.
// permissions.push describes the authenticated user, and an installation
// token has no user behind it: GitHub returns the field, and it is false
// whatever the installation holds. Under the App the installation's own
// contents permission decides.
func (p *Processor) readWriteAccess(ctx context.Context, owner, repo string) (bool, error) {
	key := owner + "/" + repo

	if p.AppWriteAccess != nil {
		allowed, err := p.AppWriteAccess(ctx, owner, repo)
		if err != nil {
			return false, fmt.Errorf("write access check on %s: %w", key, err)
		}
		return allowed, nil
	}

	repository, _, err := p.Client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return false, fmt.Errorf("write access check on %s: %w", key, err)
	}
	permissions := repository.GetPermissions()
	if permissions == nil || permissions.Push == nil {
		return false, fmt.Errorf("%w: %s", errWriteAccessUnknown, key)
	}
	return *permissions.Push, nil
}

// writeAccessDetail renders an ensureWriteAccess failure for status output. A
// proven denial reads as a refusal; a failed or unanswered lookup must not,
// because the two need different repairs.
func writeAccessDetail(err error) string {
	switch {
	case errors.Is(err, errNoWriteAccess):
		return "approve refused: " + err.Error()
	case errors.Is(err, errWriteAccessUnknown):
		return "write access unknown: " + err.Error()
	default:
		return ghErrorDetail("write access check error", err)
	}
}

// accessCache is the per-Processor memo used by ensureWriteAccess. It lives
// in its own struct so Processor literals in tests stay zero-value friendly.
type accessCache struct {
	writeAccess memo[bool]
}
