package github

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

// The refusals go-github returns for a call made outside a stoppable run,
// GET /user and the discovery among them, read as the same exhaustion.
func TestAsRateLimitReadsGoGitHubRefusals(t *testing.T) {
	reset := time.Date(2026, 10, 13, 12, 4, 50, 0, time.UTC)
	wait := 90 * time.Second

	tests := map[string]struct {
		err  error
		want func(t *testing.T, got *RateLimitExhausted)
	}{
		"primary": {
			err: fmt.Errorf("getting authenticated user: %w", &github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: reset}}}),
			want: func(t *testing.T, got *RateLimitExhausted) {
				require.Equal(t, reset, got.Reset)
			},
		},
		"secondary": {
			err: &github.AbuseRateLimitError{RetryAfter: &wait},
			want: func(t *testing.T, got *RateLimitExhausted) {
				require.WithinDuration(t, time.Now().Add(wait), got.Reset, 5*time.Second)
			},
		},
		"wrapped exhaustion": {
			err: fmt.Errorf("searching PRs: %w", &RateLimitExhausted{Reset: reset}),
			want: func(t *testing.T, got *RateLimitExhausted) {
				require.Equal(t, reset, got.Reset)
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := AsRateLimit(t.Context(), tc.err)
			require.True(t, ok)
			tc.want(t, got)
		})
	}
}

func TestAsRateLimitIgnoresOtherFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, ok := AsRateLimit(ctx, errors.New("not found"))
	require.False(t, ok, "a cancelled run is not a rate refusal")
	_, ok = AsRateLimit(t.Context(), &github.AbuseRateLimitError{})
	require.False(t, ok, "a secondary refusal that names no wait gives no reset")
}

func TestRateLimitExhaustedMessage(t *testing.T) {
	err := &RateLimitExhausted{Reset: time.Date(2026, 10, 13, 12, 4, 50, 0, time.FixedZone("CEST", 2*3600))}
	require.Equal(t, "GitHub rate limit reached for this token; it resets at 2026-10-13T10:04:50Z", err.Error())
}
