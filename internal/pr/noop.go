package pr

import (
	"regexp"
	"strings"

	"github.com/google/go-github/v92/github"
)

// Renovate keeps the human-readable version of a pinned GitHub Actions
// reference in a trailing comment:
//
//	- uses: actions/checkout@fbc6f39...c09 # v5
//
// When upstream moves the tag without moving the commit, Renovate opens a PR
// that only rewrites that comment:
//
//	-      - uses: actions/checkout@fbc6f39...c09 # v5
//	+      - uses: actions/checkout@fbc6f39...c09 # v5.1.0
//
// The workflow runs the pinned commit either way, so the PR changes nothing
// that executes. Such a PR is worth closing, never worth rescuing, and it is
// the kind of entry that gets a rescue agent pointed at it when it shows up
// as a conflict.

// pinnedUsesRE matches a workflow step that pins its action to a full commit
// SHA, with the trailing comment separated out. Only a 40-character hex ref
// counts: a tag or branch ref carries the version itself, so a change to it
// is a real change.
var pinnedUsesRE = regexp.MustCompile(`^\s*-?\s*uses:\s*\S+@[0-9a-fA-F]{40}\s*(#.*)?$`)

// workflowPathRE matches the YAML files whose trailing version comments this
// rule knows about: GitHub workflow and composite-action definitions.
var workflowPathRE = regexp.MustCompile(`^\.github/(workflows|actions)/.+\.ya?ml$`)

// NoOpDiff reports whether a PR's changed files carry no semantic change:
// every changed line is a pinned-SHA `uses:` line in a GitHub workflow whose
// pinned commit is unchanged and whose trailing version comment is all that
// moved.
//
// The verdict is conservative, because a wrong "no-op" hides a real change:
// an empty file list, a file outside .github/workflows or .github/actions, a
// patch GitHub truncated or omitted, an unequal number of added and removed
// lines, or one changed line that is not a pinned `uses:` all make it false.
//
// Within a file the removed and added lines are paired by order. A pair of
// unrelated lines can only make the verdict stricter, because samePinnedAction
// accepts a pair only when the two lines are equal once the comment is gone.
func NoOpDiff(files []*github.CommitFile) bool {
	if len(files) == 0 {
		return false
	}
	changed := false
	for _, f := range files {
		if !workflowPathRE.MatchString(f.GetFilename()) {
			return false
		}
		patch := f.GetPatch()
		if patch == "" {
			// A binary file, or a diff GitHub did not deliver: unknown, so
			// not a no-op.
			return false
		}
		removed, added := changedLines(patch)
		if len(removed) != len(added) || len(removed) == 0 {
			return false
		}
		for i := range removed {
			if !samePinnedAction(removed[i], added[i]) {
				return false
			}
		}
		changed = true
	}
	return changed
}

// changedLines splits a unified diff into its removed and added lines, both
// without the leading marker and in file order. The "\ No newline at end of
// file" marker and the hunk headers are not changes.
//
// A GitHub CommitFile patch carries no "---" or "+++" file header, so a line
// that starts that way is file content: a removed YAML document marker is a
// removed line like any other.
func changedLines(patch string) (removed, added []string) {
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "\\"):
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line[1:])
		case strings.HasPrefix(line, "+"):
			added = append(added, line[1:])
		}
	}
	return removed, added
}

// samePinnedAction reports whether two `uses:` lines pin the same action to
// the same commit and differ only in their trailing comment.
func samePinnedAction(before, after string) bool {
	if !pinnedUsesRE.MatchString(before) || !pinnedUsesRE.MatchString(after) {
		return false
	}
	return stripTrailingComment(before) == stripTrailingComment(after)
}

// stripTrailingComment removes a trailing "# ..." comment and the whitespace
// in front of it. Leading whitespace is kept, so a re-indented line is a
// real change.
func stripTrailingComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	return strings.TrimRight(line, " \t")
}
