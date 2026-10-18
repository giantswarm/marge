package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
)

// TestFanOut holds what every run reads and writes under: the reads go as
// wide as GitHub answers, or as the policy where it declares wider, and the
// writes keep the policy's bounds.
func TestFanOut(t *testing.T) {
	defaults := pr.CompanyDefaults().Concurrency

	tests := []struct {
		name       string
		file       string
		wantReads  pr.Concurrency
		wantWrites pr.Concurrency
	}{
		{"company defaults", "", pr.ReadConcurrency, defaults},
		{"a policy wider than the read bound", "concurrency:\n  perTeam: 20\n", pr.Concurrency{PerTeam: 20, PerRepo: pr.ReadConcurrency.PerRepo}, pr.Concurrency{PerTeam: 20, PerRepo: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := policy.ParseDocument(policy.DefaultFile, tt.file)
			require.NoError(t, err)
			set, err := policy.NewSet([]policy.File{{Path: policy.DefaultFile, Doc: doc}}, nil)
			require.NoError(t, err)

			reads, writes := fanOut(RunOptions{Policies: set})

			require.Equal(t, tt.wantReads, reads)
			require.Equal(t, tt.wantWrites, writes)
		})
	}
}
