package remedy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

func TestCloseClosesThePR(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"number": 1, "state": "closed"}`))
	}))
	defer server.Close()

	req := botRequest(apiClient(t, server))
	req.Pull.State = new("open")

	out, err := Default().Apply(t.Context(), Close, req, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.Equal(t, "closed", body["state"])
}

// An Align files PR is regenerated every cycle, so closing it only makes the
// bot open it again.
func TestCloseRefusesAnAlignFilesPR(t *testing.T) {
	req := botRequest(nil)
	req.Kind = pr.KindAlignFiles
	req.Pull.User = &github.User{Login: new("giantswarm-align-files[bot]")}

	out, err := Default().Apply(t.Context(), Close, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "regenerated, not closed")
}

func TestCloseRefusesAnAlreadyClosedPR(t *testing.T) {
	req := botRequest(nil)
	req.Pull.State = new("closed")

	out, err := Default().Apply(t.Context(), Close, req, nil)

	require.NoError(t, err)
	require.Equal(t, "the PR is already closed", out.Refused)
}

func TestMarkWaitKeepsTheClassification(t *testing.T) {
	out, err := Default().Apply(t.Context(), MarkWait, botRequest(nil), nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.True(t, out.KeepClassification)
}

func TestDispatchAlignWorkflowNamesTheRepository(t *testing.T) {
	var path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	out, err := Default().Apply(t.Context(), DispatchAlignWorkflow, botRequest(apiClient(t, server)), nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.True(t, out.KeepClassification)
	require.Equal(t, "/repos/giantswarm/github/actions/workflows/align-files.yaml/dispatches", path)
	require.Equal(t, "main", body["ref"])
	require.Equal(t, map[string]any{"repository": "marge"}, body["inputs"])
}

const protection = `{"strict": true, "contexts": ["go-build", "go-test", "package and push alfred-app chart"]}`

func protectionServer(t *testing.T, after string) (*httptest.Server, *[]byte) {
	t.Helper()
	var written []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			written, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(after))
			return
		}
		_, _ = w.Write([]byte(protection))
	}))
	return server, &written
}

func TestFixProtectionContextDropsTheStaleContexts(t *testing.T) {
	const after = `{"strict": true, "contexts": ["go-build", "go-test"]}`
	server, written := protectionServer(t, after)
	defer server.Close()

	req := botRequest(apiClient(t, server))
	req.Required.Missing = []string{"package and push alfred-app chart"}

	out, err := Default().Apply(t.Context(), FixProtectionContext, req, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.Contains(t, out.Detail, "package and push alfred-app chart")

	var sent map[string]any
	require.NoError(t, json.Unmarshal(*written, &sent))
	require.Equal(t, []any{"go-build", "go-test"}, sent["contexts"])
	require.NotContains(t, sent, "strict", "the write never changes strict")
}

// A branch left with no required check is weaker than the one the migration
// left behind, so that decision stays with a person.
func TestFixProtectionContextRefusesToEmptyTheProtection(t *testing.T) {
	server, _ := protectionServer(t, protection)
	defer server.Close()

	req := botRequest(apiClient(t, server))
	req.Required.Missing = []string{"go-build", "go-test", "package and push alfred-app chart"}

	out, err := Default().Apply(t.Context(), FixProtectionContext, req, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.Contains(t, out.Refused, "would leave main with none")
}

func TestFixProtectionContextRefusesWithNothingMissing(t *testing.T) {
	out, err := Default().Apply(t.Context(), FixProtectionContext, botRequest(nil), nil)

	require.NoError(t, err)
	require.Equal(t, "every required context reported", out.Refused)
}

// The write is read back. A context still required afterwards stops the
// sweep for that repository rather than reporting a fix that did not happen.
func TestFixProtectionContextReadsTheWriteBack(t *testing.T) {
	server, _ := protectionServer(t, protection)
	defer server.Close()

	req := botRequest(apiClient(t, server))
	req.Required.Missing = []string{"package and push alfred-app chart"}

	out, err := Default().Apply(t.Context(), FixProtectionContext, req, nil)

	require.Error(t, err)
	require.True(t, out.StopRepository)
}
