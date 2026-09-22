package process

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// A repository whose base branch is strict drains at one PR per run. The
// sweep merges the PR exactly as it did before and names the setting that
// holds the rest of the queue.
func TestFinding_strictProtectionIsNamed(t *testing.T) {
	f := greenFixture()
	f.strict = true
	got := f.run(t, nil)

	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.NotNil(t, got.Finding)
	require.Equal(t, pr.FindingStrictProtection, got.Finding.Cause)
}

// require_code_owner_reviews refuses the sweep's own approval outright,
// while strict protection only delays a merge, so a repository with both
// reports the rule that holds the queue.
func TestFinding_codeOwnerReviewComesBeforeStrict(t *testing.T) {
	f := greenFixture()
	f.strict = true
	f.codeOwnerReviews = true
	got := f.run(t, nil)

	require.NotNil(t, got.Finding)
	require.Equal(t, pr.FindingCodeOwnerReview, got.Finding.Cause)
	require.Empty(t, got.Finding.Detail)
}

// A repository whose settings hold nothing back reports nothing.
func TestFinding_noSettingIsNoFinding(t *testing.T) {
	got := greenFixture().run(t, nil)

	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.Nil(t, got.Finding)
}

// A protection the caller may not read yields no finding: the sweep must
// never report a setting it could not see.
func TestFinding_unreadableProtectionIsNoFinding(t *testing.T) {
	f := greenFixture()
	f.protectionForbidden = true
	got := f.run(t, nil)

	require.Nil(t, got.Finding)
}

// A required context that reported nothing, on a head whose other checks
// finished long ago, will not report at all. The PR still waits, and the
// finding names the context so the fix reaches the repository.
func TestFinding_silentRequiredContextIsNamed(t *testing.T) {
	f := greenFixture()
	f.required = []string{"go-build", "check-values-schema / validate"}
	f.checksSettledAt = time.Now().Add(-90 * 24 * time.Hour)
	got := f.run(t, nil)

	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.NotNil(t, got.Finding)
	require.Equal(t, pr.FindingSilentContext, got.Finding.Cause)
	require.Equal(t, "check-values-schema / validate", got.Finding.Detail)
}

// A context that has not reported on a head whose checks finished an hour
// ago may still report. That is a wait, and nothing more.
func TestFinding_recentlySettledIsOnlyAWait(t *testing.T) {
	f := greenFixture()
	f.required = []string{"go-build", "ci/circleci: release"}
	f.checksSettledAt = time.Now().Add(-time.Hour)
	got := f.run(t, nil)

	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.Nil(t, got.Finding)
}

// A head that still has a check running says nothing about a context that
// has not reported: the run may report it yet.
func TestFinding_pendingCheckIsOnlyAWait(t *testing.T) {
	f := greenFixture()
	f.required = []string{"go-build", "ci/circleci: release"}
	f.headChecks["lint"] = "pending"
	f.checksSettledAt = time.Now().Add(-90 * 24 * time.Hour)
	got := f.run(t, nil)

	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.Nil(t, got.Finding)
}

// A dry run reports the same findings as a sweep that writes: the teams
// measured their queues with --dry-run, and a report only that run cannot
// produce is one nobody reads before turning the sweep on.
func TestFinding_dryRunReportsTheSameSetting(t *testing.T) {
	f := greenFixture()
	f.strict = true
	got := f.run(t, func(p *Processor) { p.DryRun = true })

	require.Equal(t, pr.StatusEligible, got.State, got.Detail)
	require.NotNil(t, got.Finding)
	require.Equal(t, pr.FindingStrictProtection, got.Finding.Cause)
}
