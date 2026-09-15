package process

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
)

// guardFixture wires a fake GitHub API for one bot PR (org/repo#7, base
// "main") whose head checks, base checks, branch protection, merge answer
// and author are chosen per scenario. Writes land in the fixture's own
// state, so a second ProcessPR over the same fixture reads back what the
// first one wrote, and every write is counted.
type guardFixture struct {
	author         string
	title          string
	body           string
	mergeableState string
	autoMerge      bool
	fork           bool
	// headChecks maps a check name on the PR head to "success", "failure",
	// "pending" or "neutral"; statusContexts lists the names reported as
	// commit statuses instead of check runs.
	headChecks     map[string]string
	statusContexts []string
	// baseChecks maps a check name on the base head to its conclusion; a
	// name that is absent never ran there.
	baseChecks map[string]string
	// required lists the protection's required contexts; nil means the
	// branch has no required checks (GitHub answers 404).
	required []string
	strict   bool
	// mergeRefusal, when set, is the 405 message GitHub answers the merge
	// with instead of merging.
	mergeRefusal   string
	labelForbidden bool

	mu       sync.Mutex
	labels   []string
	comments []string

	approveCalls      atomic.Int32
	mergeCalls        atomic.Int32
	updateBranchCalls atomic.Int32
	labelAdds         atomic.Int32
	labelRemoves      atomic.Int32
	commentPosts      atomic.Int32
	protectionWrites  atomic.Int32
}

const (
	gfHead = "ccc333ccc333ccc333ccc333ccc333ccc333ccc3"
	gfBase = "ddd444ddd444ddd444ddd444ddd444ddd444ddd4"
)

func greenFixture() *guardFixture {
	return &guardFixture{
		author:         "renovate[bot]",
		title:          "chore(deps): update all non-major dependencies",
		mergeableState: "clean",
		headChecks:     map[string]string{"go-build": "success", "lint": "success"},
		baseChecks:     map[string]string{"go-build": "success", "lint": "success"},
		required:       []string{"go-build"},
	}
}

func (f *guardFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	isStatusContext := func(name string) bool {
		for _, c := range f.statusContexts {
			if c == name {
				return true
			}
		}
		return false
	}
	checksFor := func(checks map[string]string, includeStatuses bool) (github.CombinedStatus, github.ListCheckRunsResults) {
		cs := github.CombinedStatus{State: new("success")}
		runs := github.ListCheckRunsResults{Total: new(0)}
		for name, state := range checks {
			if includeStatuses && isStatusContext(name) {
				cs.Statuses = append(cs.Statuses, &github.RepoStatus{Context: new(name), State: new(state)})
				if state == "failure" {
					cs.State = new("failure")
				}
				continue
			}
			cr := &github.CheckRun{ID: new(int64(len(runs.CheckRuns) + 1)), Name: new(name), Status: new("completed"), Conclusion: new(state)}
			if state == "pending" {
				cr.Status = new("in_progress")
				cr.Conclusion = nil
			}
			runs.CheckRuns = append(runs.CheckRuns, cr)
		}
		runs.Total = new(len(runs.CheckRuns))
		return cs, runs
	}

	mux.HandleFunc("GET /repos/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, github.Repository{Name: new("repo"), Permissions: &github.RepositoryPermissions{Push: new(true), Pull: new(true)}})
	})
	mux.HandleFunc("GET /repos/org/repo/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		labels := make([]*github.Label, 0, len(f.labels))
		for _, l := range f.labels {
			labels = append(labels, &github.Label{Name: l})
		}
		f.mu.Unlock()
		pull := github.PullRequest{
			Number:         new(7),
			Title:          new(f.title),
			Body:           new(f.body),
			ChangedFiles:   new(1),
			MergeableState: new(f.mergeableState),
			User:           &github.User{Login: new(f.author)},
			Head:           &github.PullRequestBranch{SHA: new(gfHead), Ref: new("renovate/all"), Repo: &github.Repository{Fork: new(f.fork)}},
			Base:           &github.PullRequestBranch{SHA: new(gfBase), Ref: new("main")},
			Labels:         labels,
		}
		if f.autoMerge {
			pull.AutoMerge = &github.PullRequestAutoMerge{MergeMethod: new("squash")}
		}
		writeJSON(w, pull)
	})

	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/7/head/status", func(w http.ResponseWriter, r *http.Request) {
		cs, _ := checksFor(f.headChecks, true)
		writeJSON(w, cs)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/refs/pull/7/head/check-runs", func(w http.ResponseWriter, r *http.Request) {
		_, runs := checksFor(f.headChecks, true)
		writeJSON(w, runs)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+gfBase+"/status", func(w http.ResponseWriter, r *http.Request) {
		cs, _ := checksFor(f.baseChecks, true)
		writeJSON(w, cs)
	})
	mux.HandleFunc("GET /repos/org/repo/commits/"+gfBase+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
		_, runs := checksFor(f.baseChecks, true)
		writeJSON(w, runs)
	})

	mux.HandleFunc("GET /repos/org/repo/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		if f.required == nil {
			http.NotFound(w, r)
			return
		}
		checks := make([]*github.RequiredStatusCheck, 0, len(f.required))
		for _, c := range f.required {
			checks = append(checks, &github.RequiredStatusCheck{Context: c})
		}
		writeJSON(w, github.RequiredStatusChecks{Strict: f.strict, Checks: &checks})
	})
	for _, route := range []string{
		"DELETE /repos/org/repo/branches/main/protection/enforce_admins",
		"POST /repos/org/repo/branches/main/protection/enforce_admins",
		"PUT /repos/org/repo/branches/main/protection",
		"DELETE /repos/org/repo/branches/main/protection",
	} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			f.protectionWrites.Add(1)
			t.Errorf("guard violated: the sweep wrote branch protection: %s %s", r.Method, r.URL.Path)
			http.Error(w, "forbidden by test", http.StatusForbidden)
		})
	}

	mux.HandleFunc("GET /repos/org/repo/compare/main..."+gfHead, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, github.CommitsComparison{
			Status:     new("ahead"),
			AheadBy:    new(1),
			BaseCommit: &github.RepositoryCommit{SHA: new(gfBase)},
			Files:      []*github.CommitFile{{Filename: new("go.mod"), Status: new("modified"), Patch: new("@@ -1 +1 @@\n-a\n+b")}},
		})
	})

	mux.HandleFunc("GET /repos/org/repo/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := make([]*github.IssueComment, 0, len(f.comments))
		for i, body := range f.comments {
			out = append(out, &github.IssueComment{ID: new(int64(i + 1)), Body: new(body)})
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /repos/org/repo/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		var req github.IssueCommentRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.comments = append(f.comments, req.Body)
		f.mu.Unlock()
		f.commentPosts.Add(1)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, github.IssueComment{ID: new(int64(99))})
	})

	mux.HandleFunc("GET /repos/org/repo/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []*github.PullRequestReview{})
	})
	mux.HandleFunc("POST /repos/org/repo/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		f.approveCalls.Add(1)
		writeJSON(w, github.PullRequestReview{State: new("APPROVED")})
	})
	mux.HandleFunc("PUT /repos/org/repo/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) {
		f.mergeCalls.Add(1)
		if f.mergeRefusal != "" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			writeJSON(w, map[string]string{"message": f.mergeRefusal})
			return
		}
		writeJSON(w, github.PullRequestMergeResult{Merged: new(true)})
	})
	mux.HandleFunc("PUT /repos/org/repo/pulls/7/update-branch", func(w http.ResponseWriter, r *http.Request) {
		f.updateBranchCalls.Add(1)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, github.PullRequestBranchUpdateResponse{Message: new("Updating pull request branch.")})
	})

	mux.HandleFunc("POST /repos/org/repo/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		f.labelAdds.Add(1)
		if f.labelForbidden {
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			return
		}
		var names []string
		_ = json.NewDecoder(r.Body).Decode(&names)
		f.mu.Lock()
		f.labels = append(f.labels, names...)
		f.mu.Unlock()
		writeJSON(w, []*github.Label{})
	})
	mux.HandleFunc("DELETE /repos/org/repo/issues/7/labels/{name...}", func(w http.ResponseWriter, r *http.Request) {
		f.labelRemoves.Add(1)
		name, _ := url.PathUnescape(r.PathValue("name"))
		require.Equal(t, "bot-prs-sweep/pending", name, "the label name reaches GitHub unescaped")
		f.mu.Lock()
		kept := f.labels[:0]
		for _, l := range f.labels {
			if l != name {
				kept = append(kept, l)
			}
		}
		f.labels = kept
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /repos/org/repo/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, github.Label{})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	return httptest.NewServer(mux)
}

// run processes org/repo#7 once through a full sweep (every action, no
// wait) and returns its final entry. configure may narrow the processor.
func (f *guardFixture) run(t *testing.T, configure func(*Processor)) pr.StatusEntry {
	t.Helper()
	server := f.server(t)
	defer server.Close()
	proc := NewProcessor(newTestClient(t, server), false, false, "me")
	if configure != nil {
		configure(proc)
	}
	status := pr.NewPRStatus()
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 7, Author: f.author}
	idx := status.Add(info)
	proc.ProcessPR(context.Background(), info, status, idx)
	return status.Snapshot()[idx]
}

func (f *guardFixture) labelSet() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.labels...)
}

// TestGuard_requiredCheckMissingIsAWait: a required context nobody reported
// is not green. The sweep waits and writes nothing but the label, even
// though every reported check passed.
func TestGuard_requiredCheckMissingIsAWait(t *testing.T) {
	f := greenFixture()
	f.required = []string{"go-build", "ci/circleci: release"}
	got := f.run(t, nil)

	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.Contains(t, got.Detail, "ci/circleci: release")
	require.Zero(t, f.mergeCalls.Load())
	require.Zero(t, f.approveCalls.Load())
	require.Equal(t, []string{"bot-prs-sweep/pending"}, f.labelSet())
}

func TestGuard_requiredCheckPendingIsAWait(t *testing.T) {
	f := greenFixture()
	f.headChecks["go-build"] = "pending"
	got := f.run(t, nil)

	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.Contains(t, got.Detail, "pending: go-build")
	require.Zero(t, f.mergeCalls.Load())
}

// TestGuard_requiredMatchesWorkflowJobName: a protection may list an
// Actions job as "<workflow> / <job>" while the check run reports the job
// name alone.
func TestGuard_requiredMatchesWorkflowJobName(t *testing.T) {
	f := greenFixture()
	f.required = []string{"ci / go-build"}
	got := f.run(t, nil)

	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.Equal(t, int32(1), f.mergeCalls.Load())
}

// TestGuard_strictBehindUpdatesBranchAndWaits is the strict-protection
// chain, one round per sweep: a green PR behind its base is brought up to
// date, never merged in the same sweep.
func TestGuard_strictBehindUpdatesBranchAndWaits(t *testing.T) {
	f := greenFixture()
	f.strict = true
	f.mergeableState = "behind"
	got := f.run(t, nil)

	require.Equal(t, pr.StatusRefreshed, got.State, got.Detail)
	require.Equal(t, int32(1), f.updateBranchCalls.Load())
	require.Zero(t, f.mergeCalls.Load())
	require.Equal(t, int32(1), f.approveCalls.Load())
	require.Equal(t, int32(1), f.commentPosts.Load(), "the update-branch evidence is written once")
}

// TestGuard_redNonRequiredCheck covers the main-is-green discriminator: a
// red non-required check blocks when the same check is green on the base
// head, merges (named in the evidence) when it is red there too, and blocks
// when it never ran there, because absent is not green.
func TestGuard_redNonRequiredCheck(t *testing.T) {
	cases := []struct {
		name       string
		onBase     string
		wantState  pr.StatusState
		wantMerges int32
	}{
		{"green on base blocks", "success", pr.StatusFailed, 0},
		{"red on base is pre-existing and merges", "failure", pr.StatusMerged, 1},
		{"absent on base blocks", "", pr.StatusFailed, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := greenFixture()
			f.headChecks["lint"] = "failure"
			delete(f.baseChecks, "lint")
			if tc.onBase != "" {
				f.baseChecks["lint"] = tc.onBase
			}
			got := f.run(t, nil)

			require.Equal(t, tc.wantState, got.State, got.Detail)
			require.Equal(t, tc.wantMerges, f.mergeCalls.Load())
			require.Contains(t, got.Detail, "lint")
			if tc.wantState == pr.StatusMerged {
				require.Equal(t, int32(1), f.commentPosts.Load())
				require.Contains(t, f.comments[0], "merged-past-red-check")
				require.Contains(t, f.comments[0], "lint")
			}
		})
	}
}

// TestGuard_redRequiredCheckIsAFailure: a red required check is a failure
// whatever the base says.
func TestGuard_redRequiredCheckIsAFailure(t *testing.T) {
	f := greenFixture()
	f.headChecks["go-build"] = "failure"
	f.baseChecks["go-build"] = "failure"
	got := f.run(t, nil)

	require.Equal(t, pr.StatusFailed, got.State, got.Detail)
	require.Zero(t, f.mergeCalls.Load())
	require.Equal(t, []string{"bot-prs-sweep/action-required"}, f.labelSet())
}

// TestGuard_securityCheckNeverMerges: a failing security check is never
// merged past, even when it is red on the base head too, and leaves a
// blocked marker that a rescue reads.
func TestGuard_securityCheckNeverMerges(t *testing.T) {
	f := greenFixture()
	f.headChecks["gosec"] = "failure"
	f.baseChecks["gosec"] = "failure"
	got := f.run(t, nil)

	require.Equal(t, pr.StatusFailedSecurity, got.State, got.Detail)
	require.Zero(t, f.mergeCalls.Load())
	require.Zero(t, f.approveCalls.Load())
	require.Equal(t, []string{"bot-prs-sweep/security"}, f.labelSet())
	require.Equal(t, int32(1), f.commentPosts.Load())
	marker := pr.ParseRescueMarker(f.comments[0])
	require.NotNil(t, marker)
	require.Equal(t, "blocked", marker.Outcome)
	require.False(t, marker.IsEvidence(), "a security block is a rescue-visible marker")
}

// TestGuard_onlyTrustedBotsAreSwept: a person's PR, the caller's own
// included, is never approved or merged.
func TestGuard_onlyTrustedBotsAreSwept(t *testing.T) {
	for _, author := range []string{"quentin", "me", "renovate"} {
		t.Run(author, func(t *testing.T) {
			f := greenFixture()
			f.author = author
			got := f.run(t, nil)

			require.Equal(t, pr.StatusUntrustedAuthor, got.State, got.Detail)
			require.Zero(t, f.approveCalls.Load())
			require.Zero(t, f.mergeCalls.Load())
			require.Equal(t, []string{"bot-prs-sweep/skipped"}, f.labelSet())
		})
	}
}

// TestGuard_neverWritesBranchProtection: a full green flow, approve and
// merge included, touches no protection endpoint. The fixture fails the
// test on any such write.
func TestGuard_neverWritesBranchProtection(t *testing.T) {
	f := greenFixture()
	got := f.run(t, nil)

	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.Equal(t, int32(1), f.approveCalls.Load())
	require.Equal(t, int32(1), f.mergeCalls.Load())
	require.Zero(t, f.protectionWrites.Load())
	require.Equal(t, []string{"bot-prs-sweep/merged"}, f.labelSet())
}

// TestGuard_autoMergeIsObservedOnly: GitHub merges what auto-merge is set
// on; the sweep neither approves nor merges it.
func TestGuard_autoMergeIsObservedOnly(t *testing.T) {
	f := greenFixture()
	f.autoMerge = true
	got := f.run(t, nil)

	require.Equal(t, pr.StatusAutoMerge, got.State, got.Detail)
	require.Zero(t, f.approveCalls.Load())
	require.Zero(t, f.mergeCalls.Load())
	require.Equal(t, []string{"bot-prs-sweep/auto-merge"}, f.labelSet())
}

// TestGuard_eligibility applies the company defaults: majors and unreadable
// updates are held for a person; lockfile and grouped non-major updates
// merge; Align files and Herald PRs always merge.
func TestGuard_eligibility(t *testing.T) {
	cases := []struct {
		name      string
		author    string
		title     string
		body      string
		wantState pr.StatusState
	}{
		{"renovate major is held", "renovate[bot]", "fix(deps): update module sigs.k8s.io/cluster-api to v2", "| a | `v1.14.2` → `v2.0.0` |", pr.StatusHeld},
		{"renovate unreadable is held", "renovate[bot]", "chore(deps): update codemirror", "", pr.StatusHeld},
		{"renovate patch merges", "renovate[bot]", "fix(deps): update module foo to v1.2.4", "| a | `v1.2.3` → `v1.2.4` |", pr.StatusMerged},
		{"renovate lockfile merges", "renovate[bot]", "chore(deps): lock file maintenance", "", pr.StatusMerged},
		{"dependabot major is held", "dependabot[bot]", "Bump lodash from 3.10.1 to 4.17.21", "", pr.StatusHeld},
		{"align files merges", "giantswarm-align-files[bot]", "chore: align files according to platform standards", "", pr.StatusMerged},
		{"herald merges", "heraldbot[bot]", "fix(nancy): remediate findings on main", "", pr.StatusMerged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := greenFixture()
			f.author, f.title, f.body = tc.author, tc.title, tc.body
			got := f.run(t, nil)

			require.Equal(t, tc.wantState, got.State, got.Detail)
			if tc.wantState == pr.StatusHeld {
				require.Zero(t, f.mergeCalls.Load())
				require.Zero(t, f.approveCalls.Load())
				require.Contains(t, got.Detail, "held")
				require.Equal(t, []string{"bot-prs-sweep/action-required"}, f.labelSet())
			} else {
				require.Equal(t, int32(1), f.mergeCalls.Load())
			}
		})
	}
}

// TestGuard_mergeRefusedForReviewIsAwaitingApproval: when the sweep's own
// approval does not satisfy the review rule, GitHub's refusal is recorded
// and nothing escalates to an admin merge.
func TestGuard_mergeRefusedForReviewIsAwaitingApproval(t *testing.T) {
	f := greenFixture()
	f.mergeRefusal = "At least 1 approving review is required by reviewers with write access."
	got := f.run(t, nil)

	require.Equal(t, pr.StatusAwaitingApproval, got.State, got.Detail)
	require.Equal(t, int32(1), f.mergeCalls.Load())
	require.Zero(t, f.protectionWrites.Load())
	require.Equal(t, []string{"bot-prs-sweep/awaiting-approval"}, f.labelSet())
	require.Equal(t, int32(1), f.commentPosts.Load())
	require.Contains(t, f.comments[0], "awaiting-approval")
}

func TestGuard_mergeRefusedForChecksIsAWait(t *testing.T) {
	f := greenFixture()
	f.mergeRefusal = `Required status check "release" is expected.`
	got := f.run(t, nil)

	require.Equal(t, pr.StatusWaitingChecks, got.State, got.Detail)
	require.Equal(t, []string{"bot-prs-sweep/pending"}, f.labelSet())
}

// TestGuard_twoSweepsWriteOnce: the second sweep over an unchanged PR
// re-reads the label and the marker it wrote and writes neither again.
func TestGuard_twoSweepsWriteOnce(t *testing.T) {
	f := greenFixture()
	f.headChecks["gosec"] = "failure"
	server := f.server(t)
	defer server.Close()
	proc := NewProcessor(newTestClient(t, server), false, false, "me")
	info := pr.PRInfo{Owner: "org", Repo: "repo", Number: 7, Author: f.author}

	for sweep := 1; sweep <= 2; sweep++ {
		status := pr.NewPRStatus()
		idx := status.Add(info)
		proc.ProcessPR(context.Background(), info, status, idx)
		require.Equal(t, pr.StatusFailedSecurity, status.Snapshot()[idx].State)
	}

	require.Equal(t, int32(1), f.labelAdds.Load(), "the label is written once")
	require.Zero(t, f.labelRemoves.Load())
	require.Equal(t, int32(1), f.commentPosts.Load(), "the marker is written once")
	require.Equal(t, []string{"bot-prs-sweep/security"}, f.labelSet())
}

// TestGuard_labelReplacedWhenClassChanges: one classification label per
// PR, the previous one removed.
func TestGuard_labelReplacedWhenClassChanges(t *testing.T) {
	f := greenFixture()
	f.labels = []string{"bot-prs-sweep/pending", "renovate"}
	got := f.run(t, nil)

	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.Equal(t, int32(1), f.labelRemoves.Load())
	require.ElementsMatch(t, []string{"renovate", "bot-prs-sweep/merged"}, f.labelSet())
}

// TestGuard_dryRunWritesNothing: every outcome is decided and reported,
// nothing is written, labels included.
func TestGuard_dryRunWritesNothing(t *testing.T) {
	f := greenFixture()
	got := f.run(t, func(p *Processor) { p.DryRun = true })

	require.Equal(t, pr.StatusSkipped, got.State, got.Detail)
	require.Contains(t, got.Detail, "would approve, merge (squash)")
	require.Zero(t, f.approveCalls.Load()+f.mergeCalls.Load()+f.labelAdds.Load()+f.commentPosts.Load())
}

// TestGuard_classifyOnlyLabelsAndStops: --actions classify writes the
// label and nothing else.
func TestGuard_classifyOnlyLabelsAndStops(t *testing.T) {
	f := greenFixture()
	got := f.run(t, func(p *Processor) { p.Actions = ActionSet{ActionClassify: true} })

	require.Equal(t, pr.StatusEligible, got.State, got.Detail)
	require.Zero(t, f.approveCalls.Load()+f.mergeCalls.Load()+f.commentPosts.Load())
	require.Equal(t, []string{"bot-prs-sweep/eligible"}, f.labelSet())
}

// TestGuard_labelForbiddenNeverChangesTheOutcome: a label the caller may
// not write is a note on the entry, not a different result.
func TestGuard_labelForbiddenNeverChangesTheOutcome(t *testing.T) {
	f := greenFixture()
	f.labelForbidden = true
	got := f.run(t, nil)

	require.Equal(t, pr.StatusMerged, got.State, got.Detail)
	require.Contains(t, got.Detail, "label not set")
}

func TestGuard_forkIsSkipped(t *testing.T) {
	f := greenFixture()
	f.fork = true
	got := f.run(t, nil)

	require.Equal(t, pr.StatusSkipped, got.State, got.Detail)
	require.Zero(t, f.approveCalls.Load()+f.mergeCalls.Load())
}

func TestParseActions(t *testing.T) {
	all, err := ParseActions("")
	require.NoError(t, err)
	for _, a := range AllActions {
		require.True(t, all.Has(a), string(a))
	}

	some, err := ParseActions(" Merge ,approve")
	require.NoError(t, err)
	require.Equal(t, "classify,approve,merge", some.String())
	require.False(t, some.Has(ActionRefresh))

	_, err = ParseActions("rescue")
	require.Error(t, err)
	require.Contains(t, err.Error(), "classify, approve, merge, refresh, retry, mark")
	require.True(t, strings.Contains(err.Error(), fmt.Sprintf("%q", "rescue")))
}
