package process

import (
	"fmt"
	"slices"
	"strings"
)

// A failing check that never produced a verdict on the code is not a
// failure. Two shapes are recognised here, both distinct from the Actions
// budget block in budget.go:
//
//   - the job was cancelled, so it built, tested and scanned nothing. A
//     rerun is the remedy.
//   - CircleCI refuses the pipeline because setup workflows are disabled for
//     the repository. No rerun and no branch fix helps; somebody must change
//     the setting.
//
// Both land in the same bucket, out of the failure buckets, because the
// operator reads the remedy from the detail either way. Reporting them as
// failures is expensive for a security check in particular: "Security
// failures" is the one category an operator never waves through, so a
// gitleaks job that was killed before it scanned anything draws manual
// attention and discourages the rerun that actually fixes it.

// noVerdictCheck is one failing check that established nothing about the
// code, with the reason that says so.
type noVerdictCheck struct {
	Name   string
	Reason string
}

// conclusionCancelled is the check-run conclusion GitHub reports for a job
// that was stopped before it finished. It is reported alongside "failure"
// and settles the question on its own: a killed job scanned nothing.
//
// "timed_out" is deliberately not treated the same way. GitHub sets it when
// a job exceeds its time limit, which usually means the job ran and hung, so
// it stays on the failure path where a hanging test is visible.
const conclusionCancelled = "cancelled"

const cancelledReason = "cancelled before it finished"

// setupWorkflowPhrase is the distinctive part of the message CircleCI
// reports when a repository uses a setup workflow while the project setting
// that allows it is off: "Use of setup workflows must be enabled in project
// settings (Project settings > Advanced -> Dynamic config using setup
// workflows)". The full sentence is not matched because a commit-status
// description is capped at 140 characters and arrives truncated.
const setupWorkflowPhrase = "setup workflows must be enabled"

// circleCISetupReason names the setting so the operator does not have to
// read the CircleCI logs to find it.
const circleCISetupReason = "CircleCI setup workflows disabled; enable Project settings > Advanced > Dynamic config using setup workflows"

// isSetupWorkflowBlock reports whether msg is the CircleCI setup-workflow
// project-setting refusal.
func isSetupWorkflowBlock(msg string) bool {
	return msg != "" && strings.Contains(strings.ToLower(msg), setupWorkflowPhrase)
}

// matchesAnyMessage reports whether any of a check run's output fields or
// annotation messages satisfies match. It is pure so every classification
// rule can be exercised without hitting the GitHub API.
func matchesAnyMessage(match func(string) bool, title, summary, text string, annotationMessages []string) bool {
	return match(title) || match(summary) || match(text) ||
		slices.ContainsFunc(annotationMessages, match)
}

// noVerdictDetail builds the detail string for a PR whose failing checks
// established nothing, naming each check and why.
func noVerdictDetail(checks []noVerdictCheck) string {
	if len(checks) == 0 {
		return "checks produced no verdict"
	}
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = fmt.Sprintf("%s (%s)", c.Name, c.Reason)
	}
	return fmt.Sprintf("checks produced no verdict: %s", joinCapped(parts))
}
