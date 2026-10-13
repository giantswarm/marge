package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
)

// TestClassifyOnly holds what widens the fan-out: the classify step alone,
// and nothing that writes. A set that carries one write step is a sweep, and
// keeps the policy's own bounds.
func TestClassifyOnly(t *testing.T) {
	classify, err := process.ParseActions("classify")
	require.NoError(t, err)
	require.True(t, classify.ClassifyOnly())

	withApprove, err := process.ParseActions("classify,approve")
	require.NoError(t, err)
	require.False(t, withApprove.ClassifyOnly())

	every, err := process.ParseActions("")
	require.NoError(t, err)
	require.False(t, every.ClassifyOnly())

	require.False(t, process.ActionSet(nil).ClassifyOnly(), "a nil set performs every action")
	require.False(t, process.ActionSet{}.ClassifyOnly(), "an empty set classifies nothing")
}

// TestFanOut holds what each kind of run reads and writes under: a classify
// run reads wide whether or not it labels, its label writes keep the
// policy's product, and a sweep keeps the policy's bounds for everything.
func TestFanOut(t *testing.T) {
	policyBound := pr.CompanyDefaults().Concurrency
	classify := process.ActionSet{process.ActionClassify: true}
	sweep := process.ActionSet{process.ActionClassify: true, process.ActionApprove: true, process.ActionMerge: true}

	tests := []struct {
		name            string
		opts            RunOptions
		wantConcurrency pr.Concurrency
		wantLabelWrites int
	}{
		{"classify dry run", RunOptions{DryRun: true, Actions: classify}, pr.ReadConcurrency, 0},
		{"classify writing labels", RunOptions{Actions: classify}, pr.ReadConcurrency, policyBound.PerTeam * policyBound.PerRepo},
		{"sweep", RunOptions{Actions: sweep}, policyBound, 0},
		{"sweep dry run", RunOptions{DryRun: true, Actions: sweep}, policyBound, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			concurrency, labelWrites := fanOut(tt.opts)

			require.Equal(t, tt.wantConcurrency, concurrency)
			if tt.wantLabelWrites == 0 {
				require.Nil(t, labelWrites)
				return
			}
			require.Equal(t, tt.wantLabelWrites, cap(labelWrites))
		})
	}
}

// TestListRequestIsClassifyOnly ties the list tool to that: a refresh is the
// classify step on a dry run, which is what lets it read wide.
func TestListRequestIsClassifyOnly(t *testing.T) {
	req, err := listRequest(call(map[string]any{"team": "bumblebee", "refresh": true}))

	require.NoError(t, err)
	require.True(t, req.Opts.DryRun)
	require.True(t, req.Opts.Actions.ClassifyOnly())
}
