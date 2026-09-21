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

// commentServer answers the comment listing with standing and records
// every comment the sweep writes.
func commentServer(t *testing.T, standing []*github.IssueComment, written *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/giantswarm/marge/issues/1/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(standing)
	})
	mux.HandleFunc("POST /repos/giantswarm/marge/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		var body github.IssueCommentRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		*written = append(*written, body.GetBody())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(github.IssueComment{ID: new(int64(1))})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// unhandledProcessor records unhandled failures and writes markers.
func unhandledProcessor(t *testing.T, srv *httptest.Server) *Processor {
	t.Helper()
	return &Processor{
		Login:   "marge",
		Client:  newTestClient(t, srv),
		Actions: ActionSet{ActionClassify: true, ActionRemedy: true, ActionMark: true},
	}
}

// unhandledRun is a failing PR whose log excerpt a rule already fetched.
func unhandledRun(excerpt string) *prRun {
	run := remedyRun(pr.StatusFailed)
	run.commentsLoaded = false
	run.excerpts = map[string]string{"actions go-build": excerpt}
	return run
}

// A failure no rule recognises leaves the signature on the PR. The run's
// own grouping ends with the process, so the marker is what a later
// command reads.
func TestUnhandledFailureIsMarkedOnThePR(t *testing.T) {
	var written []string
	proc := unhandledProcessor(t, commentServer(t, nil, &written))
	run := unhandledRun("go: updates to go.mod needed; to update it:\n\tgo mod tidy\n")

	proc.recordUnhandled(t.Context(), run)

	entry := run.status.Snapshot()[run.idx]
	require.NotNil(t, entry.Unhandled)
	require.Len(t, written, 1)

	marker := pr.ParseRescueMarker(written[0])
	require.NotNil(t, marker)
	require.True(t, marker.IsUnhandled())
	require.Equal(t, entry.Unhandled.Signature, marker.Signature)
	require.Equal(t, []string{"go-build"}, marker.Checks)
	require.Contains(t, marker.Excerpt, "go mod tidy")
	require.Contains(t, written[0], pr.UnhandledPhrase)
}

// The marker is written once per change: a second run over the same PR,
// with the same head, adds no second comment.
func TestUnhandledMarkerIsWrittenOncePerChange(t *testing.T) {
	var written []string
	standing := &pr.RescueMarker{
		Tool: markerTool, Kind: pr.MarkerKindEvidence, Outcome: pr.MarkerOutcomeUnhandled,
		Signature: "abc123abc123", Checks: []string{"go-build"}, Excerpt: "recorded earlier",
		HeadSHA: "headsha",
	}
	proc := unhandledProcessor(t, commentServer(t, []*github.IssueComment{{Body: new(standing.CommentBody())}}, &written))

	proc.recordUnhandled(t.Context(), unhandledRun("a different excerpt of the same failure"))

	require.Empty(t, written, "a marker already stands for this change")
}

// A signature belongs to the change, not to the run: a later run whose log
// excerpt differs reports the signature the standing marker carries, so one
// failure stays one group.
func TestUnhandledSignatureComesFromTheStandingMarker(t *testing.T) {
	var written []string
	standing := &pr.RescueMarker{
		Tool: markerTool, Kind: pr.MarkerKindEvidence, Outcome: pr.MarkerOutcomeUnhandled,
		Signature: "abc123abc123", Checks: []string{"go-build"}, Excerpt: "recorded earlier",
		HeadSHA: "headsha",
	}
	proc := unhandledProcessor(t, commentServer(t, []*github.IssueComment{{Body: new(standing.CommentBody())}}, &written))
	run := unhandledRun("")

	proc.recordUnhandled(t.Context(), run)

	entry := run.status.Snapshot()[run.idx]
	require.Equal(t, "abc123abc123", entry.Unhandled.Signature)
	require.Equal(t, "recorded earlier", entry.Unhandled.Excerpt)
}

// A marker written for another change says nothing about the one on the
// branch, so the run records its own signature and writes it.
func TestStaleUnhandledMarkerIsReplaced(t *testing.T) {
	var written []string
	standing := &pr.RescueMarker{
		Tool: markerTool, Kind: pr.MarkerKindEvidence, Outcome: pr.MarkerOutcomeUnhandled,
		Signature: "abc123abc123", Checks: []string{"go-build"}, Excerpt: "recorded earlier",
		HeadSHA: "an-older-head",
	}
	proc := unhandledProcessor(t, commentServer(t, []*github.IssueComment{{Body: new(standing.CommentBody())}}, &written))
	run := unhandledRun("go: updates to go.mod needed\n")

	proc.recordUnhandled(t.Context(), run)

	entry := run.status.Snapshot()[run.idx]
	require.NotEqual(t, "abc123abc123", entry.Unhandled.Signature)
	require.Len(t, written, 1)
}

// A PR with no failing check carries no unhandled failure, so nothing is
// recorded and nothing is written.
func TestNoFailingCheckWritesNoMarker(t *testing.T) {
	var written []string
	proc := unhandledProcessor(t, commentServer(t, nil, &written))
	run := unhandledRun("")
	run.failing = nil

	proc.recordUnhandled(t.Context(), run)

	require.Nil(t, run.status.Snapshot()[run.idx].Unhandled)
	require.Empty(t, written)
}

// The live case of giantswarm/mcp-capi#223: the check that sorts first
// returned the output of a step that succeeded, and the failure is in the
// log of the check beside it.
func TestUnhandledSignatureSkipsAStepThatSucceeded(t *testing.T) {
	var written []string
	proc := unhandledProcessor(t, commentServer(t, nil, &written))
	run := unhandledRun("")
	run.failing = []string{"ci/circleci: go-test", "ci/circleci: go-build"}
	run.excerpts = map[string]string{
		"circleci ci/circleci: go-build": "==> go-build\nBuilding mcp-capi-linux-amd64...\n",
		"circleci ci/circleci: go-test":  "--- FAIL: TestSweep (0.01s)\nExited with code exit status 1\n",
	}

	proc.recordUnhandled(t.Context(), run)

	entry := run.status.Snapshot()[run.idx]
	require.Contains(t, entry.Unhandled.Excerpt, "--- FAIL: TestSweep")
}

// No excerpt names a failure, so the first one stands: a signature over a
// weak excerpt still groups the PRs that carry it.
func TestUnhandledSignatureFallsBackToTheFirstExcerpt(t *testing.T) {
	var written []string
	proc := unhandledProcessor(t, commentServer(t, nil, &written))
	run := unhandledRun("")
	run.failing = []string{"go-test", "go-build"}
	run.excerpts = map[string]string{
		"actions go-build": "Building...\n",
		"actions go-test":  "ok  \tgithub.com/giantswarm/marge\n",
	}

	proc.recordUnhandled(t.Context(), run)

	entry := run.status.Snapshot()[run.idx]
	require.Equal(t, "Building...\n", entry.Unhandled.Excerpt)
}

// An excerpt of a check the classification did not find failing never
// signs the failure.
func TestUnhandledSignatureIgnoresACheckThatIsNotFailing(t *testing.T) {
	var written []string
	proc := unhandledProcessor(t, commentServer(t, nil, &written))
	run := unhandledRun("")
	run.failing = []string{"go-build"}
	run.excerpts = map[string]string{
		"actions go-build": "Building...\n",
		"actions lint":     "##[error]Process completed with exit code 1.\n",
	}

	proc.recordUnhandled(t.Context(), run)

	entry := run.status.Snapshot()[run.idx]
	require.Equal(t, "Building...\n", entry.Unhandled.Excerpt)
}
