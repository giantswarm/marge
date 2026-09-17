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
