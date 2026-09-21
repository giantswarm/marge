package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
