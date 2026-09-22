package process

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// heimdallText is the message the gate writes on a bot PR whose e2e suite
// nobody started, recorded from giantswarm/external-dns-app#482 on
// 2026-09-22.
const heimdallText = "## Details for commit: `8940190`\n\n" +
	"ℹ️ App E2E tests are required for every provider configured in `./tests/e2e/config.yaml` and in the per-suite configs under `tests/e2e/suites`: `capa`\n\n" +
	"⚠️ Check Run `App E2E Test Suites - capa` is required but wasn't found - you can trigger it by commenting on the PR with `/run app-test-suites-single PROVIDER=capa`\n"

// A gate that never finishes is the only signal such a PR carries: it is
// reported, so it is not a missing context, and it produced no verdict, so
// it is not a failing check. The message it reports has to survive into the
// subject or no rule can read it.
func TestPendingChecksCarryTheirMessage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(github.CombinedStatus{
			SHA:      new("8940190"),
			State:    new("pending"),
			Statuses: []*github.RepoStatus{},
		})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(github.ListCheckRunsResults{
			Total: new(2),
			CheckRuns: []*github.CheckRun{
				{
					Name:   new("Heimdall - PR Gatekeeper"),
					Status: new("in_progress"),
					Output: &github.CheckRunOutput{
						Title:   new("Heimdall - PR Gatekeeper"),
						Summary: new("🚧 PR currently blocked from merging"),
						Text:    new(heimdallText),
					},
				},
				{
					Name:       new("check-values-schema / validate"),
					Status:     new("completed"),
					Conclusion: new("success"),
				},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	processor := &Processor{Client: newTestClient(t, server)}

	outcome, err := processor.getCombinedCheckState(t.Context(), pr.PRInfo{Owner: "org", Repo: "repo", Number: 1})

	require.NoError(t, err)
	require.Equal(t, statePending, outcome.state)
	require.Len(t, outcome.pendingChecks, 1)
	require.Equal(t, "Heimdall - PR Gatekeeper", outcome.pendingChecks[0].Name)
	require.Contains(t, outcome.pendingChecks[0].Output, "/run app-test-suites-single PROVIDER=capa")
	// The title and the summary are one message with the text, so a pattern
	// anchored on any of the three reads the same string.
	require.Contains(t, outcome.pendingChecks[0].Output, "blocked from merging")
	// A finished check says nothing about what is still to start.
	require.NotContains(t, outcome.pendingChecks[0].Name, "check-values-schema")
}

// A check run that reports no message is still pending. The rule then finds
// nothing to capture and leaves the PR alone, which is a decision the
// matcher makes, not a check the collection skips.
func TestPendingChecksWithoutAMessage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(github.CombinedStatus{SHA: new("8940190"), State: new("pending")})
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/1/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(github.ListCheckRunsResults{
			Total:     new(1),
			CheckRuns: []*github.CheckRun{{Name: new("Heimdall - PR Gatekeeper"), Status: new("queued")}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	processor := &Processor{Client: newTestClient(t, server)}

	outcome, err := processor.getCombinedCheckState(t.Context(), pr.PRInfo{Owner: "org", Repo: "repo", Number: 1})

	require.NoError(t, err)
	require.Len(t, outcome.pendingChecks, 1)
	require.Empty(t, outcome.pendingChecks[0].Output)
}
