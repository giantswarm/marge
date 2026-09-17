package cmd

import (
	"fmt"
	"strings"
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

	section(&b, "Merged", result.Merged)
	section(&b, "Remedied", result.Remedied)
	section(&b, "Refreshed", result.Refreshed)
	section(&b, "Retried", result.Retried)
	section(&b, "Blocked, security", result.SecurityFailures)
	section(&b, "Blocked", result.ActionRequired)

	unhandledSection(&b, result.Unhandled)

	if counts.Skipped > 0 {
		fmt.Fprintf(&b, "%s skipped by policy.\n", plural(counts.Skipped, "PR"))
	}
	for _, failure := range result.RepositoriesFailed {
		fmt.Fprintf(&b, "Repository %s could not be listed: %s\n", failure.Repo, failure.Error)
	}
	if result.Rules != nil && result.Rules.Error != "" {
		fmt.Fprintf(&b, "Rules unavailable (%s): %s. Every remedy was refused.\n", result.Rules.Source, result.Rules.Error)
	}
	return strings.TrimRight(b.String(), "\n"), true
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
	named := make([]string, 0, len(headlineParts(counts)))
	accounted := 0
	for _, part := range headlineParts(counts) {
		accounted += part.count
		if text := countPart(part.count, part.name); text != "" {
			named = append(named, text)
		}
	}
	if other := counts.Total - accounted; other > 0 {
		named = append(named, countPart(other, "other"))
	}
	return strings.Join(named, ", ")
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
	return fmt.Sprintf("%d %ss", count, noun)
}
