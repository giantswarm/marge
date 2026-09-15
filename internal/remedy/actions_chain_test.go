package remedy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// chainServer records every request the chain makes, so a test can assert
// what it wrote and, more importantly, what it did not.
type chainServer struct {
	mu    sync.Mutex
	calls []string
	*httptest.Server
}

func newChainServer(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *chainServer {
	t.Helper()
	cs := &chainServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.calls = append(cs.calls, r.Method+" "+r.URL.Path)
		cs.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (c *chainServer) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// chainRegistry runs the chain, which Default holds. The hold is the gate,
// and TestDefaultHoldsTheStrictChain is what pins it.
func chainRegistry() *Registry { return NewRegistry(strictChain{}) }

func chainRequest(t *testing.T, cs *chainServer, mergeableState string) *Request {
	t.Helper()
	req := botRequest(apiClient(t, cs.Server))
	req.Deps.Login = "marge"
	req.Pull.State = new("open")
	req.Pull.MergeableState = new(mergeableState)
	req.Required.Green = []string{"go-build"}
	return req
}

func TestStrictChainApprovesAndMerges(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/merge"):
			_, _ = w.Write([]byte(`{"merged": true}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})

	out, err := chainRegistry().Apply(t.Context(), StrictChain, chainRequest(t, cs, "clean"), nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.Equal(t, "squashed and merged", out.Detail)
	require.Contains(t, cs.recorded(), "POST /repos/giantswarm/marge/pulls/1/reviews")
	require.Contains(t, cs.recorded(), "PUT /repos/giantswarm/marge/pulls/1/merge")
}

// The chain writes no branch protection. The sweep only touches a bot's PR,
// where its own approving review satisfies the review rule, so there is
// nothing for an enforce_admins lift to unblock.
func TestStrictChainWritesNoProtection(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"merged": true}`))
		}
	})

	_, err := chainRegistry().Apply(t.Context(), StrictChain, chainRequest(t, cs, "clean"), nil)

	require.NoError(t, err)
	for _, call := range cs.recorded() {
		require.NotContains(t, call, "/protection", "the chain must never write branch protection")
		require.NotContains(t, call, "enforce_admins")
	}
}

func TestStrictChainSkipsAnExistingApproval(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"state": "APPROVED", "user": {"login": "marge"}}]`))
		default:
			_, _ = w.Write([]byte(`{"merged": true}`))
		}
	})

	out, err := chainRegistry().Apply(t.Context(), StrictChain, chainRequest(t, cs, "clean"), nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.NotContains(t, cs.recorded(), "POST /repos/giantswarm/marge/pulls/1/reviews")
}

// A PR behind its base is updated and merges on a later sweep: one round per
// sweep, so the sweep never blocks on CI.
func TestStrictChainUpdatesABehindPR(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	})

	out, err := chainRegistry().Apply(t.Context(), StrictChain, chainRequest(t, cs, "behind"), nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.True(t, out.KeepClassification)
	require.Contains(t, out.Detail, "merges on a later sweep")
	require.Equal(t, []string{"PUT /repos/giantswarm/marge/pulls/1/update-branch"}, cs.recorded())
}

func TestStrictChainRefusesAConflict(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })

	out, err := chainRegistry().Apply(t.Context(), StrictChain, chainRequest(t, cs, "dirty"), nil)

	require.NoError(t, err)
	require.Contains(t, out.Refused, "merge conflict")
	require.Empty(t, cs.recorded())
}

// A required context that is pending or never reported is a wait, on this
// path as on every other.
func TestStrictChainWaitsForRequiredChecks(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })

	req := chainRequest(t, cs, "clean")
	req.Required.Missing = []string{"pre-commit"}

	out, err := chainRegistry().Apply(t.Context(), StrictChain, req, nil)

	require.NoError(t, err)
	require.Equal(t, "required checks not reported: pre-commit", out.Refused)
	require.Empty(t, cs.recorded())
}

// A merge GitHub refuses for a review reason is reported, never forced.
func TestStrictChainReportsAReviewRefusal(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/merge"):
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"message": "At least 1 approving review is required by reviewers with write access."}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})

	out, err := chainRegistry().Apply(t.Context(), StrictChain, chainRequest(t, cs, "clean"), nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "approving review is required")
}

// The chain is the one action that merges, and no measured sweep has asked
// for it. Default registers it, so a rule naming it validates, and refuses
// it, so turning it on is a Go change rather than a merged rule.
func TestDefaultHoldsTheStrictChain(t *testing.T) {
	_, known := Default().Lookup(StrictChain)
	require.True(t, known, "a rule naming the chain must validate")
	require.NotEmpty(t, Default().HeldReason(StrictChain))

	out, err := Default().Apply(t.Context(), StrictChain, chainRequest(t, newChainServer(t, func(http.ResponseWriter, *http.Request) {}), "clean"), nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "strict-chain is held")
}

// One attempt per change binds the chain like every other action. A merge
// GitHub refuses is reported, and the next sweep does not retry it.
func TestStrictChainRunsOncePerChange(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"merged": true}`))
	})
	req := chainRequest(t, cs, "clean")
	req.AppliedThisChange = map[Name]bool{StrictChain: true}

	out, err := chainRegistry().Apply(t.Context(), StrictChain, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "strict-chain already applied to this change")
	require.Empty(t, cs.recorded(), "a refused chain calls nothing")
}

// The behind-base step runs update-branch, and it answers to update-branch's
// own guards rather than skipping them.
func TestStrictChainBehindBaseKeepsUpdateBranchGuards(t *testing.T) {
	cs := newChainServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	})
	req := chainRequest(t, cs, "behind")
	req.AppliedThisChange = map[Name]bool{UpdateBranch: true}

	out, err := chainRegistry().Apply(t.Context(), StrictChain, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "update-branch already applied to this change")
	require.Empty(t, cs.recorded(), "the branch is not updated a second time")
}
