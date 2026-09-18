package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"

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

// TestListRequestIsClassifyOnly ties the list tool to that: a refresh is the
// classify step on a dry run, which is what lets it read wide.
func TestListRequestIsClassifyOnly(t *testing.T) {
	req, err := listRequest(call(map[string]any{"team": "bumblebee", "refresh": true}))

	require.NoError(t, err)
	require.True(t, req.Opts.DryRun)
	require.True(t, req.Opts.Actions.ClassifyOnly())
}
