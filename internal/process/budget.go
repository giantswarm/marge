package process

import (
	"fmt"
	"strings"
)

// Detection matches the single message GitHub actually emits when a job is
// refused before it can start because an Actions budget is exhausted.
//
// Verified against the live GitHub API (GET .../check-runs and its
// /annotations) on a real budget-blocked PR. The observed shape is:
//
//	check run: status "completed", conclusion "failure",
//	           output.title/summary/text all null, output.annotations_count 1
//	annotation: annotation_level "failure", path ".github", title "",
//	            message "The job was not started because an Actions budget is preventing further use."
//
// So the message is carried in the check run's *annotation message*, not its
// output fields, and the conclusion is "failure" (we also accept
// "startup_failure"/"timed_out"/"cancelled" defensively). We still scan the
// output fields too, in case a future variant populates them.
//
// Only this API-verified wording is matched. No unverified billing / payment /
// spending-limit variants are assumed, so any unrecognized block degrades to
// the normal Failed path rather than risking a real failure being hidden.

// budgetBlockPhrases are case-insensitive substrings that unambiguously
// identify an Actions budget block. They cannot plausibly appear in a genuine
// test/build/lint failure, so finding one anywhere is sufficient.
var budgetBlockPhrases = []string{
	"actions budget is preventing further use",
}

// isBudgetBlockMessage reports whether msg is the GitHub "Actions budget"
// block message (a job refused before starting because the budget was
// exhausted), as opposed to a real CI failure.
func isBudgetBlockMessage(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	for _, phrase := range budgetBlockPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// isBudgetBlockOutput reports whether any of a check run's output fields or
// annotation messages indicate an Actions budget block. The verified message
// lives in the annotation, but the output fields are scanned too as a cheap
// defensive measure. It is a pure helper so the classification can be
// exercised without hitting the GitHub API: the processor gathers the output
// strings and annotation messages and delegates the decision here.
func isBudgetBlockOutput(title, summary, text string, annotationMessages []string) bool {
	return matchesAnyMessage(isBudgetBlockMessage, title, summary, text, annotationMessages)
}

// blockedDetail builds a human-readable detail string for a PR whose CI
// could not run because of an Actions budget block, naming the affected
// checks when available. It mirrors failureDetail so the two buckets read
// consistently in the summary.
func blockedDetail(blockedChecks []string) string {
	if len(blockedChecks) == 0 {
		return "Actions budget exhausted; no jobs ran"
	}
	return fmt.Sprintf("Actions budget exhausted; no jobs ran: %s", joinCapped(blockedChecks))
}
