package process

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// Recorded-style compare payloads for a Renovate "typescript to v7" PR.
// tsFiles renders the package.json patch with the hunk header and context
// line placed the way a given base puts them; the changed lines stay put
// unless version changes. This is what a Renovate rebase looks like on the
// wire: same +/- lines, shifted hunk offsets, different context.
func tsFiles(version string, hunkStart int, contextDep string) []*github.CommitFile {
	offset := strconv.Itoa(hunkStart)
	patch := "@@ -" + offset + ",7 +" + offset + ",7 @@\n" +
		"   \"devDependencies\": {\n" +
		"     " + contextDep + "\n" +
		"-    \"typescript\": \"^5.9.2\",\n" +
		"+    \"typescript\": \"" + version + "\",\n" +
		"     \"vite\": \"^7.1.0\"\n   }\n }"
	return []*github.CommitFile{{
		SHA: new("blob-" + version), Filename: new("package.json"), Status: new("modified"),
		Additions: new(1), Deletions: new(1), Changes: new(2), Patch: new(patch),
	}}
}

const (
	tsTitle = "chore(deps): update dependency typescript to v7"
	// oldHead is the head SHA the marker was written against; the fixture's
	// PR head (fxHead) differs, so every marker below has "moved".
	oldHead = "0ld5ha000ld5ha000ld5ha000ld5ha000ld5ha00"
)

// markerBeforeRebase is the marker a rescue wrote on the PR before Renovate
// rebased it: pinned to the old head and to the fingerprint of the diff as
// it looked on the old base.
var markerBeforeRebase = markerCommentWith(oldHead, pr.Fingerprint{
	PatchID:  pr.PatchID(tsFiles("^7.0.0", 20, `"prettier": "^3.6.2",`), 1),
	ChangeID: "typescript@v7",
})

func TestRescueMarker_rebasedSameChangeStaysFresh(t *testing.T) {
	// Not behind (Renovate just rebased), go-build still red: a real Failed.
	// The diff is byte-identical apart from hunk offsets and context.
	f := &staleFixture{
		title: tsTitle, changedFiles: 1,
		files:    tsFiles("^7.0.0", 24, `"prettier": "^3.7.0",`),
		comments: []string{markerBeforeRebase},
	}
	got := f.run(t, nil)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed", got.State, got.Detail)
	}
	if got.Rescue == nil {
		t.Fatal("rescue marker should be attached")
	}
	if got.Rescue.Stale || !got.Rescue.Rebased {
		t.Errorf("rescue = stale %v, rebased %v; want fresh and rebased", got.Rescue.Stale, got.Rescue.Rebased)
	}
	if f.compareCalls.Load() != 2 {
		t.Errorf("compare called %d times, want 2 (stale check + fingerprint)", f.compareCalls.Load())
	}
	if ann := pr.FormatRescue(got.Rescue, got.Rescue.At.Add(5*24*time.Hour)); ann != "rescue failed 5d ago (klaus), rebased since: same change: nope" {
		t.Errorf("annotation = %q", ann)
	}
}

func TestRescueMarker_changedDiffIsStale(t *testing.T) {
	// Renovate moved the PR to typescript 7.1.0: the + line changed.
	f := &staleFixture{
		title: tsTitle, changedFiles: 1,
		files:    tsFiles("^7.1.0", 24, `"prettier": "^3.7.0",`),
		comments: []string{markerBeforeRebase},
	}
	got := f.run(t, nil)

	if got.State != pr.StatusFailed {
		t.Fatalf("state = %v (%s), want StatusFailed", got.State, got.Detail)
	}
	if got.Rescue == nil || !got.Rescue.Stale || got.Rescue.Rebased {
		t.Fatalf("rescue = %+v, want a stale, non-rebased marker", got.Rescue)
	}
}

func TestRescueMarker_versionBumpIsStale_titleFallback(t *testing.T) {
	// The lockfile diff is too large for the compare API (no patch), so the
	// patch ID is unavailable on the sweep side and the title decides.
	truncated := append(tsFiles("^7.0.0", 24, `"prettier": "^3.7.0",`), &github.CommitFile{
		Filename: new("package-lock.json"), Status: new("modified"), Changes: new(12000),
	})

	t.Run("new major in the title", func(t *testing.T) {
		f := &staleFixture{
			title: "chore(deps): update dependency typescript to v8", changedFiles: 2, files: truncated,
			comments: []string{markerBeforeRebase},
		}
		got := f.run(t, nil)
		if got.Rescue == nil || !got.Rescue.Stale {
			t.Fatalf("rescue = %+v, want stale (typescript@v7 != typescript@v8)", got.Rescue)
		}
	})

	t.Run("same title", func(t *testing.T) {
		f := &staleFixture{
			title: tsTitle, changedFiles: 2, files: truncated,
			comments: []string{markerBeforeRebase},
		}
		got := f.run(t, nil)
		if got.Rescue == nil || got.Rescue.Stale || !got.Rescue.Rebased {
			t.Fatalf("rescue = %+v, want fresh and rebased via the change id", got.Rescue)
		}
	})
}

func TestRescueMarker_legacyMarkerKeepsHeadOnlyBehaviour(t *testing.T) {
	f := &staleFixture{
		title: tsTitle, changedFiles: 1,
		files:    tsFiles("^7.0.0", 24, `"prettier": "^3.7.0",`),
		comments: []string{markerComment(oldHead[:8])},
	}
	got := f.run(t, nil)

	if got.Rescue == nil || !got.Rescue.Stale || got.Rescue.Rebased {
		t.Fatalf("rescue = %+v, want stale: a marker without fingerprint ages out with the head", got.Rescue)
	}
	if f.compareCalls.Load() != 1 {
		t.Errorf("compare called %d times, want 1: no fingerprint to compare against", f.compareCalls.Load())
	}
}

func TestRescueMarker_sameHeadNeedsNoFingerprint(t *testing.T) {
	f := &staleFixture{
		title: tsTitle, changedFiles: 1,
		files:    tsFiles("^7.0.0", 24, `"prettier": "^3.7.0",`),
		comments: []string{markerCommentWith(fxHead, pr.Fingerprint{PatchID: "irrelevant", ChangeID: "typescript@v7"})},
	}
	got := f.run(t, nil)

	if got.Rescue == nil || got.Rescue.Stale || got.Rescue.Rebased {
		t.Fatalf("rescue = %+v, want fresh and not rebased", got.Rescue)
	}
	if f.compareCalls.Load() != 1 {
		t.Errorf("compare called %d times, want 1: same head, nothing to fingerprint", f.compareCalls.Load())
	}
}

func TestClassifyStale_refreshSkippedForRebasedMarker(t *testing.T) {
	// Stale (behind, go-build green on main) but the fresh-by-fingerprint
	// marker says automation already lost on this change: no refresh.
	f := &staleFixture{
		behindBy: 12, baseConclusion: "success",
		title: tsTitle, changedFiles: 1,
		files:    tsFiles("^7.0.0", 24, `"prettier": "^3.7.0",`),
		comments: []string{markerBeforeRebase},
	}
	got := f.run(t, func(p *Processor) { p.Actions = ActionSet{ActionClassify: true, ActionRefresh: true} })

	if got.State != pr.StatusStale {
		t.Fatalf("state = %v (%s), want StatusStale (refresh skipped)", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "refresh skipped") {
		t.Errorf("detail %q should say the refresh was skipped", got.Detail)
	}
	if f.updateBranchCalls.Load() != 0 {
		t.Errorf("update-branch called %d times despite a valid (rebased) marker, want 0", f.updateBranchCalls.Load())
	}
	if got.Rescue == nil || got.Rescue.Stale || !got.Rescue.Rebased {
		t.Fatalf("rescue = %+v, want fresh and rebased", got.Rescue)
	}
}
