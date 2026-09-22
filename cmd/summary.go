package cmd

import (
	"fmt"
	"strings"

	"github.com/giantswarm/marge/internal/pr"
)

// summaryPRLimit bounds how many pull requests one section names. A team
// with fifty blocked PRs gets a readable message and a count, not a wall.
const summaryPRLimit = 10

// teamSummary renders one team's run for its Slack channel and reports
// whether the run changed anything. A run that changed nothing renders no
// text: the channel is the run history, and a channel that fills with
// "nothing happened" stops being read.
//
// Changed means marge wrote something that moves a PR on: a merge, a branch
// refresh, a build retry or a rule's remedy. A label and an evidence comment
// are written on every classification and are not, on their own, news.
func teamSummary(team string, result SweepResult) (string, bool) {
	counts := result.Summary
	changed := counts.Merged + counts.Refreshed + counts.Retried + counts.Remedied
	if changed == 0 {
		return "", false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "*%s* swept %s: %s\n", team, plural(counts.Total, "bot PR"), headline(counts))
	summaryBody(&b, result)
	return strings.TrimRight(b.String(), "\n"), true
}

// stoppedSummary renders a run whose context was cancelled before the team
// was finished, which on a scheduled run is Kubernetes at
// activeDeadlineSeconds. It renders text whether the run changed anything
// or not: the channel reads silence as "nothing changed", and a run that
// was cut short has established no such thing.
func stoppedSummary(team string, result SweepResult) string {
	counts := result.Summary
	unreached := unaccounted(counts)

	var b strings.Builder
	switch {
	case counts.Total == 0:
		fmt.Fprintf(&b, "*%s* was stopped before it read its queue, and wrote nothing.\n", team)
	case unreached <= 0:
		fmt.Fprintf(&b, "*%s* was stopped before it finished, with all %s decided: %s\n",
			team, plural(counts.Total, "bot PR"), namedOutcomes(counts))
	default:
		fmt.Fprintf(&b, "*%s* was stopped before it finished: %d of %s swept, %d never reached: %s\n",
			team, counts.Total-unreached, plural(counts.Total, "bot PR"), unreached, namedOutcomes(counts))
	}
	summaryBody(&b, result)
	if unreached > 0 {
		fmt.Fprintf(&b, "The queue was not drained. The next scheduled run takes it from here.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// summaryBody writes everything under the first line, which every summary
// shares.
func summaryBody(b *strings.Builder, result SweepResult) {
	counts := result.Summary

	// The findings come before the pull request sections because the
	// summary is trimmed from the end: a report of what holds a queue is
	// worth more than the eleventh merged PR, and a team that never reads
	// it fixes nothing in the repository.
	findingsSection(b, result.Findings)

	section(b, "Merged", result.Merged)
	section(b, "Remedied", result.Remedied)
	section(b, "Refreshed", result.Refreshed)
	section(b, "Retried", result.Retried)
	section(b, "Blocked, security", result.SecurityFailures)
	section(b, "Blocked", result.ActionRequired)

	unhandledSection(b, result.Unhandled)

	if counts.Skipped > 0 {
		fmt.Fprintf(b, "%s skipped by policy.\n", plural(counts.Skipped, "PR"))
	}
	for _, failure := range result.RepositoriesFailed {
		fmt.Fprintf(b, "Repository %s could not be listed: %s\n", failure.Repo, failure.Error)
	}
	if result.Rules != nil && result.Rules.Error != "" {
		fmt.Fprintf(b, "Rules unavailable (%s): %s. Every remedy was refused.\n", result.Rules.Source, result.Rules.Error)
	}
}

// findingRepoLimit bounds how many repositories one finding names. A team
// with 78 repositories reads the cause and the count; the run's JSON output
// carries every repository.
const findingRepoLimit = 3

// findingsSection names the repository settings that hold PRs back, the one
// on the most PRs first. One line per cause, never one per repository.
func findingsSection(b *strings.Builder, findings []SweepFinding) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(b, "\nRepository settings holding PRs (%d):\n", len(findings))
	for _, finding := range findings {
		fmt.Fprintf(b, "• %s — %s in %s: %s\n",
			findingCause(finding.Cause),
			plural(finding.PRs, "PR"),
			plural(len(finding.Repositories), "repository"),
			findingRepos(finding.Repositories))
	}
}

// findingCause reads one cause as a sentence, and says what the team must
// change, because the sweep will not.
func findingCause(cause string) string {
	switch pr.RepoFindingCause(cause) {
	case pr.FindingStrictProtection:
		return "strict branch protection, so one PR merges per run"
	case pr.FindingCodeOwnerReview:
		return "`require_code_owner_reviews`, which the sweep's own approval does not satisfy"
	case pr.FindingSilentContext:
		return fmt.Sprintf("a required check that has not reported in %d days", int(pr.SilentContextAfter.Hours()/24))
	default:
		return cause
	}
}

// findingRepos names the repositories of one finding, each with its count,
// and says how many it left out.
func findingRepos(repos []SweepFindingRepo) string {
	named := make([]string, 0, findingRepoLimit+1)
	for i, repo := range repos {
		if i == findingRepoLimit {
			named = append(named, fmt.Sprintf("and %d more", len(repos)-findingRepoLimit))
			break
		}
		line := fmt.Sprintf("%s %d", repo.Repo, repo.PRs)
		if repo.Detail != "" {
			line += fmt.Sprintf(" (%s)", repo.Detail)
		}
		named = append(named, line)
	}
	return strings.Join(named, ", ")
}

// summarySignatureLimit bounds how many signatures the summary names. The
// point of the line is the pattern worth a rule, not a catalogue of every
// one-off failure.
const summarySignatureLimit = 3

// unhandledSection names the failures no rule recognised, the one on the
// most PRs first. Each signature is what `marge rules draft <signature>`
// takes, and the sweep leaves it on every PR it counts, so the line is a
// pointer to markers that outlive the run rather than the only record of
// them.
func unhandledSection(b *strings.Builder, groups []SweepUnhandled) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintf(b, "\nUnrecognised failures (%d):\n", len(groups))
	for i, group := range groups {
		if i == summarySignatureLimit {
			fmt.Fprintf(b, "• and %d more\n", len(groups)-summarySignatureLimit)
			return
		}
		fmt.Fprintf(b, "• `%s` on %s: %s\n", group.Signature, plural(group.Count, "PR"), strings.Join(group.Checks, ", "))
	}
}

// headline is the one-line count of what the run did and what it left. Every
// outcome a PR can end in is named, and a remainder is named as well, so the
// counts always add up to the total. A line that drops a category reads as if
// the sweep lost a PR.
func headline(counts SweepSummary) string {
	named := namedOutcomes(counts)
	other := unaccounted(counts)
	switch {
	case other <= 0:
		return named
	case named == "":
		return countPart(other, "other")
	default:
		return named + ", " + countPart(other, "other")
	}
}

// namedOutcomes names each outcome that holds at least one pull request.
func namedOutcomes(counts SweepSummary) string {
	named := make([]string, 0, len(headlineParts(counts)))
	for _, part := range headlineParts(counts) {
		if text := countPart(part.count, part.name); text != "" {
			named = append(named, text)
		}
	}
	return strings.Join(named, ", ")
}

// unaccounted is how many pull requests no outcome of the summary accounts
// for. On a finished run that is a PR the sweep left in a state it does not
// name; on a stopped one it is a PR the run never reached.
func unaccounted(counts SweepSummary) int {
	accounted := 0
	for _, part := range headlineParts(counts) {
		accounted += part.count
	}
	return counts.Total - accounted
}

// headlinePart is one outcome and how many PRs ended in it.
type headlinePart struct {
	count int
	name  string
}

// headlineParts lists every disjoint outcome of SweepSummary. Failed and
// SecurityFailures are counted together as blocked, which is how a reader
// thinks of them.
func headlineParts(counts SweepSummary) []headlinePart {
	return []headlinePart{
		{counts.Merged, "merged"},
		{counts.AutoMerge, "left to auto-merge"},
		{counts.Remedied, "remedied"},
		{counts.Refreshed, "refreshed"},
		{counts.Retried, "retried"},
		{counts.Failed + counts.SecurityFailures, "blocked"},
		{counts.Stale, "stale"},
		{counts.Cancelled, "cancelled"},
		{counts.Obsolete, "obsolete"},
		{counts.CIUnavailable, "CI unavailable"},
		{counts.CINoVerdict, "no CI verdict"},
		{counts.Waiting, "waiting"},
		{counts.Skipped, "skipped"},
	}
}

func countPart(count int, name string) string {
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s", count, name)
}

// section writes one list of pull requests, each as a Slack link with the
// detail that explains its outcome.
func section(b *strings.Builder, title string, entries []SweepPREntry) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s (%d):\n", title, len(entries))
	for i, entry := range entries {
		if i == summaryPRLimit {
			fmt.Fprintf(b, "• and %d more\n", len(entries)-summaryPRLimit)
			return
		}
		fmt.Fprintf(b, "• %s\n", entryLine(entry))
	}
}

// entryLine is one pull request: a link that reads as owner/repo#number, and
// the detail marge recorded for it.
func entryLine(entry SweepPREntry) string {
	name := fmt.Sprintf("%s/%s#%d", entry.Owner, entry.Repo, entry.Number)
	line := fmt.Sprintf("<%s|%s>", entry.URL, name)
	if detail := strings.TrimSpace(entry.Detail); detail != "" {
		line += " — " + detail
	}
	return line
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "y") {
		return fmt.Sprintf("%d %sies", count, strings.TrimSuffix(noun, "y"))
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
