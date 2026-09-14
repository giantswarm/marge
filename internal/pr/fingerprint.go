package pr

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"

	"github.com/google/go-github/v92/github"
)

// Fingerprint identifies what a PR changes independently of its head SHA.
// Renovate force-pushes a rebased branch whenever its base moves, so the
// head SHA changes while the change itself stays byte-identical; a
// fingerprint lets a rescue marker survive that.
//
// PatchID is the precise fingerprint: a hash over the PR's compare diff
// normalised the way `git patch-id --stable` does, minus the context lines
// -- hunk headers and line numbers are ignored, only file paths and the
// added/removed lines count. It is empty when the diff could not be fetched
// or GitHub truncated it (see PatchID).
//
// ChangeID is the cheap approximation for dependency-update PRs: the
// dependency name and target version parsed from the PR title, e.g.
// "typescript@v7". A rebase never changes it and a new version always does,
// but a version update that keeps the title (7.0.1 -> 7.0.2 under "to v7")
// is invisible to it, so it only decides when no patch ID is available. It
// is empty when the title is not a recognisable single-dependency update.
type Fingerprint struct {
	PatchID  string `json:"patch_id,omitempty"`
	ChangeID string `json:"change_id,omitempty"`
}

// Comparable reports whether the fingerprint carries anything to compare.
func (f Fingerprint) Comparable() bool {
	return f != Fingerprint{}
}

// Matches reports whether other describes the same change as f. The patch
// ID decides whenever both sides have one; the title-derived change ID is
// the fallback for when the diff could not be fingerprinted on either side.
// Nothing comparable means "not the same" -- a marker is only kept alive on
// positive evidence.
func (f Fingerprint) Matches(other Fingerprint) bool {
	if f.PatchID != "" && other.PatchID != "" {
		return f.PatchID == other.PatchID
	}
	if f.ChangeID != "" && other.ChangeID != "" {
		return f.ChangeID == other.ChangeID
	}
	return false
}

// patchIDLen is the number of hex characters kept from the SHA-256 digest:
// 64 bits tell two versions of one PR apart and stay readable in a comment.
const patchIDLen = 16

// PatchID computes the content fingerprint of a PR from the files of its
// compare response (GET /repos/{owner}/{repo}/compare/{base}...{head}).
// changedFiles is the PR's changed_files count. The compare API returns at
// most 300 files and omits the patch of a file whose diff is too large, so
// PatchID returns "" whenever the input cannot describe the whole change
// rather than fingerprinting part of it.
//
// Per file the hash covers status, previous and current path, and every
// added or removed line of the patch in order. Hunk headers and context
// lines are skipped: both shift when the base branch changes around the
// PR's lines, which is exactly what a rebase produces. A file without a
// patch and without changed lines (binary content, a pure rename, a mode
// change) contributes its blob SHA instead.
func PatchID(files []*github.CommitFile, changedFiles int) string {
	if len(files) == 0 || (changedFiles > 0 && len(files) < changedFiles) {
		return ""
	}
	sorted := make([]*github.CommitFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetFilename() < sorted[j].GetFilename() })

	h := sha256.New()
	for _, f := range sorted {
		patch := f.GetPatch()
		if patch == "" && f.GetChanges() > 0 {
			// GitHub left out the patch because the file's diff is too
			// large; the change cannot be fingerprinted from this response.
			return ""
		}
		writeField(h, f.GetStatus())
		writeField(h, f.GetPreviousFilename())
		writeField(h, f.GetFilename())
		if patch == "" {
			writeField(h, f.GetSHA())
		}
		for _, line := range strings.Split(patch, "\n") {
			if len(line) > 0 && (line[0] == '+' || line[0] == '-') {
				writeField(h, line)
			}
		}
		writeField(h, "") // end of file
	}
	return hex.EncodeToString(h.Sum(nil))[:patchIDLen]
}

// writeField writes one NUL-terminated field into the hash so neighbouring
// fields can never run together.
func writeField(w io.Writer, s string) {
	_, _ = io.WriteString(w, s)
	_, _ = w.Write([]byte{0})
}

// ChangeID fingerprints a dependency-update PR by what it updates: the
// dependency name and the target version parsed from the PR title, e.g.
// "typescript@v7" for "chore(deps): update dependency typescript to v7".
// Titles naming no single dependency or no version ("Update all non-major
// dependencies", "Lock file maintenance") yield "".
func ChangeID(title string) string {
	dep := ExtractDependencyName(title)
	ver := ExtractTargetVersion(title)
	if dep == "" || ver == "" {
		return ""
	}
	return dep + "@" + ver
}
