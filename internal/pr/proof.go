package pr

import (
	"regexp"
	"strings"
)

var (
	// beforeMergingRe finds the phrase a proof request is written with:
	// "before merging", "before merge", "before this merges".
	beforeMergingRe = regexp.MustCompile(`(?i)\bbefore\s+(this\s+)?merg(e|es|ing)\b`)
	// taskItemRe matches a markdown task list item and captures its box and
	// its text.
	taskItemRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+\[([ xX])\]\s*(.*)$`)
	// listItemRe matches any markdown list item, ticked, unticked or plain.
	listItemRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
)

// UnprovenBeforeMerge reports whether the PR body asks for a proof before
// the PR merges and that proof is still open: an unticked task list item
// that says "before merging" itself, or one in the list that follows a line
// saying so. The text of the first open item is returned so a report can
// name it.
//
// A ticked item is a proof given. A checklist with no such line in front of
// it, like the rebase box a Renovate PR carries, asks for nothing.
func UnprovenBeforeMerge(body string) (string, bool) {
	inSection := false
	listStarted := false
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimSpace(line)

		if m := taskItemRe.FindStringSubmatch(line); m != nil {
			if m[1] == " " && (inSection || beforeMergingRe.MatchString(m[2])) {
				return strings.TrimSpace(m[2]), true
			}
			if inSection {
				listStarted = true
			}
			continue
		}
		if beforeMergingRe.MatchString(line) {
			inSection, listStarted = true, false
			continue
		}
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			inSection = false
		case listItemRe.MatchString(line):
			if inSection {
				listStarted = true
			}
		case listStarted:
			// A paragraph after the list ends the section.
			inSection = false
		}
	}
	return "", false
}
