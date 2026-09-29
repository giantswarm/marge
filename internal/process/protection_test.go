package process

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// protectionServer answers every protection route with status and body.
func protectionServer(t *testing.T, header http.Header, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for key, values := range header {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// secondaryLimitBody is how GitHub reports a secondary limit: 403 with a
// documentation_url naming it, which go-github reads as
// *AbuseRateLimitError. The status is the same 403 an unreadable protection
// answers, which is why the status alone cannot tell them apart.
const secondaryLimitBody = `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#secondary-rate-limits"}`

// primaryLimit is the exhausted-quota shape: 403 with the remaining count at
// zero, which go-github reads as *RateLimitError.
func primaryLimit() http.Header {
	return http.Header{
		"X-Ratelimit-Remaining": []string{"0"},
		"X-Ratelimit-Limit":     []string{"5000"},
		"X-Ratelimit-Reset":     []string{strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)},
	}
}

// rateLimited is one shape of a refusal for rate, as a header set and a body.
type rateLimited struct {
	header http.Header
	body   string
}

func rateLimitShapes() map[string]rateLimited {
	return map[string]rateLimited{
		"primary":   {primaryLimit(), `{"message":"API rate limit exceeded"}`},
		"secondary": {nil, secondaryLimitBody},
	}
}

// A 403 that means "you may not read this protection" is the sweep's own
// case: GitHub enforces the checks for that caller, so the required set is
// empty and the merge refusal is classified as a wait.
func TestRequiredProtectionTreatsAnAccessRefusalAsNothingRequired(t *testing.T) {
	server := protectionServer(t, nil, http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)
	processor := &Processor{Client: newTestClient(t, server)}

	prot, err := processor.requiredProtection(t.Context(), pr.PRInfo{Owner: "org", Repo: "repo"}, "main")
	require.NoError(t, err)
	require.Empty(t, prot.Contexts)
}

// A rate refusal is not an answer about the branch. Reading it as "nothing is
// required" makes every required context of that base look satisfied, and the
// memo would hold that answer for the rest of the sweep. The second call is
// what shows nothing was cached: a cached zero value would answer it without
// an error.
func TestRequiredProtectionRefusesARateLimit(t *testing.T) {
	for name, shape := range rateLimitShapes() {
		t.Run(name, func(t *testing.T) {
			server := protectionServer(t, shape.header, http.StatusForbidden, shape.body)
			processor := &Processor{Client: newTestClient(t, server)}
			info := pr.PRInfo{Owner: "org", Repo: "repo"}

			_, err := processor.requiredProtection(t.Context(), info, "main")
			require.Error(t, err)

			_, err = processor.requiredProtection(t.Context(), info, "main")
			require.Error(t, err)
		})
	}
}

// requiredReview reads the same two statuses for the same reasons, so it
// makes the same distinction.
func TestRequiredReviewRefusesARateLimit(t *testing.T) {
	for name, shape := range rateLimitShapes() {
		t.Run(name, func(t *testing.T) {
			server := protectionServer(t, shape.header, http.StatusForbidden, shape.body)
			processor := &Processor{Client: newTestClient(t, server)}
			info := pr.PRInfo{Owner: "org", Repo: "repo"}

			_, err := processor.requiredReview(t.Context(), info, "main")
			require.Error(t, err)

			_, err = processor.requiredReview(t.Context(), info, "main")
			require.Error(t, err)
		})
	}
}

// rulesServer answers the classic protection routes with classic, 404 when
// it is empty, and the branch rules route with rules, the array GitHub
// sends.
func rulesServer(t *testing.T, classic map[string]string, rules string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for route, body := range classic {
		mux.HandleFunc("GET /repos/org/repo/branches/main/protection/"+route, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	mux.HandleFunc("GET /repos/org/repo/rules/branches/main", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rules))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// devctlRuleset is the shape of the ruleset devctl sets up: no classic
// protection, the required checks and the review rule in one ruleset.
const devctlRuleset = `[
  {"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"org/repo","ruleset_id":1},
  {"type":"pull_request","ruleset_source_type":"Repository","ruleset_source":"org/repo","ruleset_id":1,
   "parameters":{"required_approving_review_count":1,"require_code_owner_review":true,"dismiss_stale_reviews_on_push":false,"require_last_push_approval":false,"required_review_thread_resolution":false}},
  {"type":"required_status_checks","ruleset_source_type":"Repository","ruleset_source":"org/repo","ruleset_id":1,
   "parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"ci/circleci: go-build"},{"context":"pre-commit"}]}}
]`

// A branch protected only by a ruleset answers the classic routes with 404.
// Reading only those made every required context of a devctl-aligned
// repository look satisfied and hid its code-owner rule.
func TestRequiredProtectionReadsRulesets(t *testing.T) {
	processor := &Processor{Client: newTestClient(t, rulesServer(t, nil, devctlRuleset))}
	info := pr.PRInfo{Owner: "org", Repo: "repo"}

	prot, err := processor.requiredProtection(t.Context(), info, "main")
	require.NoError(t, err)
	require.Equal(t, []string{"ci/circleci: go-build", "pre-commit"}, prot.Contexts)
	require.False(t, prot.Strict)

	review, err := processor.requiredReview(t.Context(), info, "main")
	require.NoError(t, err)
	require.True(t, review.CodeOwnerReviews)
}

// GitHub enforces classic protection and rulesets together, so the required
// set is their union, a context named by both counted once, and either one
// being strict makes the branch strict.
func TestRequiredProtectionUnitesClassicAndRulesets(t *testing.T) {
	classic := map[string]string{
		"required_status_checks": `{"strict":false,"checks":[{"context":"pre-commit"},{"context":"lint"}]}`,
	}
	rules := `[{"type":"required_status_checks","ruleset_source_type":"Organization","ruleset_source":"org","ruleset_id":2,
	  "parameters":{"strict_required_status_checks_policy":true,"required_status_checks":[{"context":"pre-commit"},{"context":"build"}]}}]`
	processor := &Processor{Client: newTestClient(t, rulesServer(t, classic, rules))}

	prot, err := processor.requiredProtection(t.Context(), pr.PRInfo{Owner: "org", Repo: "repo"}, "main")
	require.NoError(t, err)
	require.Equal(t, []string{"pre-commit", "lint", "build"}, prot.Contexts)
	require.True(t, prot.Strict)
}

// A rate refusal on the rules read is no more an answer than one on the
// classic read: nothing is cached and the error reaches the caller.
func TestBranchRulesRefusesARateLimit(t *testing.T) {
	for name, shape := range rateLimitShapes() {
		t.Run(name, func(t *testing.T) {
			processor := &Processor{Client: newTestClient(t, protectionServer(t, shape.header, http.StatusForbidden, shape.body))}
			info := pr.PRInfo{Owner: "org", Repo: "repo"}

			_, err := processor.branchRules(t.Context(), info, "main")
			require.Error(t, err)

			_, err = processor.branchRules(t.Context(), info, "main")
			require.Error(t, err)
		})
	}
}
