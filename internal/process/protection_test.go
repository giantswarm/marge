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
