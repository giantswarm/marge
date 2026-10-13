package github

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v92/github"
)

// RateLimitExhausted is GitHub refusing a token for rate until Reset: its
// primary limit is spent, or a secondary limit asks for a wait longer than
// the transport sits out. Every call on the token is refused until then, so
// the run that met it stops instead of reading on without evidence.
type RateLimitExhausted struct {
	Reset time.Time
}

// rateLimitPhrase opens the refusal of a token GitHub stopped serving.
const rateLimitPhrase = rateLimitPhraseBudget + " reached"

func (e *RateLimitExhausted) Error() string {
	return fmt.Sprintf("%s for this token; it resets at %s", rateLimitPhrase, e.Reset.UTC().Format(time.RFC3339))
}

type exhaustionKey struct{}

// StopOnRateLimit returns a context that is cancelled, with a
// *RateLimitExhausted as its cause, the first time a request made under it
// is refused for rate for longer than the transport waits.
func StopOnRateLimit(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	ctx = context.WithValue(ctx, exhaustionKey{}, func(reset time.Time) {
		cancel(&RateLimitExhausted{Reset: reset})
	})
	return ctx, func() { cancel(nil) }
}

// reportExhausted stops the run ctx belongs to, if StopOnRateLimit started
// one.
func reportExhausted(ctx context.Context, reset time.Time) {
	if stop, ok := ctx.Value(exhaustionKey{}).(func(time.Time)); ok {
		stop(reset)
	}
}

// AsRateLimit reports whether the run under ctx, or the error it returned,
// ended on a token refused for rate, and until when.
func AsRateLimit(ctx context.Context, err error) (*RateLimitExhausted, bool) {
	if exhausted, ok := errors.AsType[*RateLimitExhausted](context.Cause(ctx)); ok {
		return exhausted, true
	}
	if err == nil {
		return nil, false
	}
	if exhausted, ok := errors.AsType[*RateLimitExhausted](err); ok {
		return exhausted, true
	}
	if primary, ok := errors.AsType[*github.RateLimitError](err); ok {
		return &RateLimitExhausted{Reset: primary.Rate.Reset.Time}, true
	}
	if secondary, ok := errors.AsType[*github.AbuseRateLimitError](err); ok && secondary.RetryAfter != nil {
		return &RateLimitExhausted{Reset: time.Now().Add(*secondary.RetryAfter)}, true
	}
	return nil, false
}

// RateBudgetShort is a run refused before it started: the token has fewer
// requests left than the run spends at the least.
type RateBudgetShort struct {
	Remaining int
	Needed    int
	Reset     time.Time
}

func (e *RateBudgetShort) Error() string {
	return fmt.Sprintf("%s too low for this run: %d requests left of the %d it needs at the least; it resets at %s",
		rateLimitPhraseBudget, e.Remaining, e.Needed, e.Reset.UTC().Format(time.RFC3339))
}

// rateLimitPhraseBudget opens every rate refusal marge reports, and a
// caller that tells them apart from other failures matches on it.
const rateLimitPhraseBudget = "GitHub rate limit"

// CheckRateBudget refuses a run that needs more REST requests than the
// client's token has left. GitHub does not count the read of the budget
// against it. A budget that cannot be read refuses nothing: the run then
// stops where GitHub refuses it, as it would without the check.
func CheckRateBudget(ctx context.Context, client *github.Client, needed int) error {
	if needed <= 0 {
		return nil
	}
	limits, _, err := client.RateLimit.Get(ctx)
	if err != nil {
		return nil
	}
	core := limits.GetCore()
	if core == nil || core.Remaining >= needed {
		return nil
	}
	return &RateBudgetShort{Remaining: core.Remaining, Needed: needed, Reset: core.Reset.Time}
}
