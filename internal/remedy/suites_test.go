package remedy

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSuiteBudgetTakesAllOrNone(t *testing.T) {
	budget := NewSuiteBudget(2)

	require.False(t, budget.Take(3))
	require.True(t, budget.Take(2))
	require.False(t, budget.Take(1))
}

func TestSuiteBudgetGiveReturnsWhatWasTaken(t *testing.T) {
	budget := NewSuiteBudget(1)
	require.True(t, budget.Take(1))

	budget.Give(1)

	require.True(t, budget.Take(1))
}

func TestSuiteBudgetOfZeroStartsNothing(t *testing.T) {
	require.False(t, NewSuiteBudget(0).Take(1))
}

func TestNilSuiteBudgetBoundsNothing(t *testing.T) {
	var budget *SuiteBudget

	require.True(t, budget.Take(100))
}

func TestSuiteBudgetNeverOvershootsUnderConcurrentTakes(t *testing.T) {
	budget := NewSuiteBudget(5)
	var taken atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if budget.Take(1) {
				taken.Add(1)
			}
		})
	}
	wg.Wait()

	require.EqualValues(t, 5, taken.Load())
}

// A run past its budget writes nothing, and the refusal leaves the PR for a
// later run.
func TestCommentCommandRefusesOnceTheRunsBudgetIsSpent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s: a spent budget must not comment", r.Method, r.URL.Path)
	}))
	defer server.Close()

	req := gateRequest(apiClient(t, server), "/run cluster-test-suites")
	req.Deps.Suites = NewSuiteBudget(0)

	out, err := Default().Apply(t.Context(), CommentCommand, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "suite budget is spent")
}

// One comment starts every provider it names, so it fits the budget whole or
// not at all.
func TestCommentCommandRefusesACommentLargerThanWhatIsLeft(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s: a comment past the budget must not be posted", r.Method, r.URL.Path)
	}))
	defer server.Close()

	budget := NewSuiteBudget(1)
	req := gateRequest(apiClient(t, server),
		"/run app-test-suites-single PROVIDER=capa",
		"/run app-test-suites-single PROVIDER=capz")
	req.Deps.Suites = budget

	out, err := Default().Apply(t.Context(), CommentCommand, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.True(t, budget.Take(1), "a refused comment takes nothing")
}

func TestCommentCommandSpendsTheBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer server.Close()

	budget := NewSuiteBudget(1)
	req := gateRequest(apiClient(t, server), "/run cluster-test-suites")
	req.Deps.Suites = budget

	out, err := Default().Apply(t.Context(), CommentCommand, req, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.False(t, budget.Take(1))
}

// A comment GitHub refused started nothing, so its suites go back to the run.
func TestCommentCommandGivesBackWhatAFailedCommentTook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	budget := NewSuiteBudget(1)
	req := gateRequest(apiClient(t, server), "/run cluster-test-suites")
	req.Deps.Suites = budget

	_, err := Default().Apply(t.Context(), CommentCommand, req, nil)

	require.Error(t, err)
	require.True(t, budget.Take(1))
}
