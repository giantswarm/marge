package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/notify"
	"github.com/giantswarm/marge/internal/pr"
)

// The summary names the failures no rule recognised, with the signature
// `marge rules draft` takes. The sweep leaves the same signature on every
// PR it counts, so the line points at markers that outlive the run.
func TestSummaryNamesTheUnrecognisedFailures(t *testing.T) {
	result := SweepResult{
		Summary: SweepSummary{Total: 4, Merged: 1},
		Unhandled: []SweepUnhandled{
			{Signature: "aaaaaaaaaaaa", Count: 3, Checks: []string{"go-build"}},
			{Signature: "bbbbbbbbbbbb", Count: 1, Checks: []string{"ci/circleci: build"}},
		},
	}

	text, changed := teamSummary("bumblebee", result)

	require.True(t, changed)
	require.Contains(t, text, "Unrecognised failures (2)")
	require.Contains(t, text, "`aaaaaaaaaaaa` on 3 PRs: go-build")
	require.Contains(t, text, "`bbbbbbbbbbbb` on 1 PR: ci/circleci: build")
}

// Only the top signatures are named: the line is a pointer to the pattern
// worth a rule, not a catalogue of every one-off failure.
func TestSummaryBoundsTheSignaturesItNames(t *testing.T) {
	result := SweepResult{Summary: SweepSummary{Total: 9, Merged: 1}}
	for _, signature := range []string{"aaa", "bbb", "ccc", "ddd", "eee"} {
		result.Unhandled = append(result.Unhandled, SweepUnhandled{Signature: signature, Count: 1, Checks: []string{"go-build"}})
	}

	text, _ := teamSummary("bumblebee", result)

	require.Equal(t, 3, strings.Count(text, "` on 1 PR"))
	require.Contains(t, text, "and 2 more")
}

// A run that recognised every failure says nothing about signatures.
func TestSummaryWithoutUnrecognisedFailures(t *testing.T) {
	text, _ := teamSummary("bumblebee", SweepResult{Summary: SweepSummary{Total: 1, Merged: 1}})
	require.NotContains(t, text, "Unrecognised failures")
}

// A stopped run says how far it got. The counts of such a run cover the PRs
// it reached, so the line names the PRs it never reached instead of folding
// them into "other", and it says the queue was not drained.
func TestStoppedSummaryNamesWhatTheRunNeverReached(t *testing.T) {
	result := SweepResult{
		Summary: SweepSummary{Total: 259, Merged: 12, Failed: 108},
		Merged:  []SweepPREntry{{Owner: "giantswarm", Repo: "external-secrets", Number: 225, Status: "Merged"}},
	}

	text := stoppedSummary("honeybadger", result)

	require.Contains(t, text, "*honeybadger* was stopped before it finished")
	require.Contains(t, text, "120 of 259 bot PRs swept")
	require.Contains(t, text, "139 never reached")
	require.Contains(t, text, "12 merged")
	require.NotContains(t, text, "139 other")
	require.Contains(t, text, "The queue was not drained")
	require.Contains(t, text, "external-secrets")
}

// A run stopped before it decided anything still posts. The team otherwise
// reads the silence as "nothing changed", which is the one thing the run
// did not establish.
func TestStoppedSummaryWithoutAnyOutcome(t *testing.T) {
	text := stoppedSummary("honeybadger", SweepResult{})

	require.Contains(t, text, "*honeybadger* was stopped before it read its queue")
	require.NotContains(t, text, "The queue was not drained")
}

// A stopped run that happened to decide every PR it found says so, and
// names no unreached PR it cannot point at.
func TestStoppedSummaryWithEveryPRDecided(t *testing.T) {
	result := SweepResult{Summary: SweepSummary{Total: 3, Merged: 3}}

	text := stoppedSummary("honeybadger", result)

	require.Contains(t, text, "with all 3 bot PRs decided")
	require.Contains(t, text, "3 merged")
	require.NotContains(t, text, "never reached")
	require.NotContains(t, text, "The queue was not drained")
}

// A run that reached no PR at all still reads as one list, with no stray
// separator in front of the remainder.
func TestHeadlineOfARunThatNamedNoOutcome(t *testing.T) {
	require.Equal(t, "4 other", headline(SweepSummary{Total: 4}))
	require.Equal(t, "2 merged, 3 other", headline(SweepSummary{Total: 5, Merged: 2}))
}

// The summary names the repository settings that hold PRs back, grouped by
// cause with a count. Honey Badger sweeps 78 repositories, so one line per
// repository never reaches a reader.
func TestSummaryNamesTheRepositorySettings(t *testing.T) {
	result := SweepResult{
		Summary: SweepSummary{Total: 20, Merged: 2},
		Findings: []SweepFinding{
			{Cause: "code_owner_review", PRs: 16, Repositories: []SweepFindingRepo{
				{Repo: "giantswarm/policy-api", PRs: 3},
				{Repo: "giantswarm/trivy-app", PRs: 2},
				{Repo: "giantswarm/crossplane", PRs: 1},
				{Repo: "giantswarm/step-exec-lib", PRs: 1},
			}},
			{Cause: "silent_required_context", PRs: 1, Repositories: []SweepFindingRepo{
				{Repo: "giantswarm/app-operator", PRs: 1, Detail: "check-values-schema / validate"},
			}},
		},
	}

	text, changed := teamSummary("honeybadger", result)

	require.True(t, changed)
	require.Contains(t, text, "Repository settings holding PRs (2)")
	require.Contains(t, text, "`require_code_owner_reviews`")
	require.Contains(t, text, "16 PRs in 4 repositories")
	require.Contains(t, text, "giantswarm/policy-api 3")
	require.Contains(t, text, "and 1 more")
	require.Contains(t, text, "1 PR in 1 repository")
	require.Contains(t, text, "giantswarm/app-operator 1 (check-values-schema / validate)")
}

// A run that found no such setting says nothing about one.
func TestSummaryWithoutRepositorySettings(t *testing.T) {
	text, _ := teamSummary("bumblebee", SweepResult{Summary: SweepSummary{Total: 1, Merged: 1}})
	require.NotContains(t, text, "Repository settings")
}

// A notice is trimmed by line to the gateway's bound, so a report that
// arrives after 3000 characters of merged PRs never reaches the team. The
// findings survive a run large enough to trim.
func TestSummaryKeepsTheFindingsInsideTheNotice(t *testing.T) {
	result := SweepResult{
		Summary: SweepSummary{Total: 300, Merged: 120, Failed: 100},
		Findings: []SweepFinding{
			{Cause: "strict_protection", PRs: 74, Repositories: strictRepos(78)},
			{Cause: "code_owner_review", PRs: 16, Repositories: strictRepos(11)},
		},
	}
	for i := range 120 {
		result.Merged = append(result.Merged, SweepPREntry{
			Owner: "giantswarm", Repo: fmt.Sprintf("a-repository-with-a-long-name-%d", i), Number: i,
			URL:    fmt.Sprintf("https://github.com/giantswarm/a-repository-with-a-long-name-%d/pull/%d", i, i),
			Detail: "merged past a red check that is red on the base head too",
		})
	}
	for i := range 100 {
		result.ActionRequired = append(result.ActionRequired, SweepPREntry{
			Owner: "giantswarm", Repo: fmt.Sprintf("another-repository-with-a-long-name-%d", i), Number: i,
			URL:    fmt.Sprintf("https://github.com/giantswarm/another-repository-with-a-long-name-%d/pull/%d", i, i),
			Detail: "go-build failed and no rule of the catalogue recognised the failure",
		})
	}

	text, changed := teamSummary("honeybadger", result)
	require.True(t, changed)
	require.Greater(t, len(text), notify.TextMax, "the test is worthless unless the summary is trimmed")

	posted := notify.Fit(text)
	require.LessOrEqual(t, len(posted), notify.TextMax)
	require.Contains(t, posted, "Repository settings holding PRs (2)")
	require.Contains(t, posted, "strict branch protection")
	require.Contains(t, posted, "`require_code_owner_reviews`")
	require.Contains(t, posted, "74 PRs in 78 repositories")
}

func strictRepos(count int) []SweepFindingRepo {
	repos := make([]SweepFindingRepo, 0, count)
	for i := range count {
		repos = append(repos, SweepFindingRepo{Repo: fmt.Sprintf("giantswarm/a-repository-with-a-long-name-%d", i), PRs: 1})
	}
	return repos
}

// The findings are grouped by cause and then by repository, the cause on
// the most PRs first, so the summary counts what a reader acts on.
func TestGroupFindingsCountsByCauseAndRepository(t *testing.T) {
	entry := func(repo string, number int, finding *pr.RepoFinding) pr.StatusEntry {
		return pr.StatusEntry{
			PR:      pr.PRInfo{Owner: "giantswarm", Repo: repo, Number: number},
			Finding: finding,
		}
	}
	strict := &pr.RepoFinding{Cause: pr.FindingStrictProtection}
	silent := &pr.RepoFinding{Cause: pr.FindingSilentContext, Detail: "check-values-schema / validate"}

	got := groupFindings([]pr.StatusEntry{
		entry("app-operator", 1580, silent),
		entry("architect", 1, strict),
		entry("architect", 2, strict),
		entry("apptest", 3, strict),
	})

	require.Len(t, got, 2)
	require.Equal(t, "strict_protection", got[0].Cause)
	require.Equal(t, 3, got[0].PRs)
	require.Equal(t, []SweepFindingRepo{
		{Repo: "giantswarm/architect", PRs: 2},
		{Repo: "giantswarm/apptest", PRs: 1},
	}, got[0].Repositories)
	require.Equal(t, "silent_required_context", got[1].Cause)
	require.Equal(t, "check-values-schema / validate", got[1].Repositories[0].Detail)
}
