package pr

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	colPR     = 8
	colStatus = 24
)

type TableColumn struct {
	Label string
	Width int
	Fn    func(PRInfo) string
	// Color optionally returns an ANSI color prefix for a value. The
	// value is padded to Width first, then wrapped, so colored cells
	// stay aligned with the rest of the table.
	Color func(PRInfo) string
}

func RepoInfoFunc(p PRInfo) string {
	return fmt.Sprintf("%s/%s", p.Owner, p.Repo)
}

func DependencyInfoFunc(p PRInfo) string {
	dep := ExtractDependencyName(p.Title)
	if dep == "" {
		return p.Title
	}
	return dep
}

func VersionInfoFunc(p PRInfo) string {
	return ExtractVersion(p.Title)
}

func AuthorInfoFunc(p PRInfo) string {
	return p.Author
}

func ageColumn() TableColumn {
	return TableColumn{Label: "Age", Width: 4, Fn: AgeInfoFunc, Color: AgeColorFunc}
}

func FullColumns() []TableColumn {
	return []TableColumn{
		{Label: "Repository", Width: 22, Fn: RepoInfoFunc},
		{Label: "Dependency", Width: 22, Fn: DependencyInfoFunc},
		{Label: "Version", Width: 18, Fn: VersionInfoFunc},
		ageColumn(),
		{Label: "Author", Width: 16, Fn: AuthorInfoFunc},
	}
}

func RepoSelectedColumns() []TableColumn {
	return []TableColumn{
		{Label: "Dependency", Width: 28, Fn: DependencyInfoFunc},
		{Label: "Version", Width: 18, Fn: VersionInfoFunc},
		ageColumn(),
		{Label: "Author", Width: 16, Fn: AuthorInfoFunc},
	}
}

func DependencySelectedColumns() []TableColumn {
	return []TableColumn{
		{Label: "Repository", Width: 28, Fn: RepoInfoFunc},
		{Label: "Version", Width: 18, Fn: VersionInfoFunc},
		ageColumn(),
		{Label: "Author", Width: 16, Fn: AuthorInfoFunc},
	}
}

func MakeHyperlink(text, url string) string {
	return fmt.Sprintf("\033]8;;%s\033\\%s\033]8;;\033\\", url, text)
}

func tableWidth(cols []TableColumn) int {
	w := colPR + colStatus
	for _, c := range cols {
		w += c.Width
	}
	w += len(cols) + 1
	return w
}

func PrintTableHeader(w *os.File, cols []TableColumn) {
	parts := make([]string, 0, len(cols)+2)
	parts = append(parts, fmt.Sprintf("%-*s", colPR, "PR"))
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("%-*s", c.Width, c.Label))
	}
	parts = append(parts, fmt.Sprintf("%-*s", colStatus, "Status"))
	header := strings.Join(parts, " ")
	divider := strings.Repeat("-", tableWidth(cols))
	_, _ = fmt.Fprintln(w, header)
	_, _ = fmt.Fprintln(w, divider)
}

func PrintRow(w *os.File, e StatusEntry, cols []TableColumn) {
	prLabel := fmt.Sprintf("%-*s", colPR, fmt.Sprintf("#%d", e.PR.Number))
	prLink := MakeHyperlink(prLabel, e.PR.URL)

	parts := make([]string, 0, len(cols)+2)
	parts = append(parts, prLink)
	for _, c := range cols {
		cell := fmt.Sprintf("%-*s", c.Width, truncate(c.Fn(e.PR), c.Width))
		if c.Color != nil {
			if code := c.Color(e.PR); code != "" {
				cell = code + cell + "\033[0m"
			}
		}
		parts = append(parts, cell)
	}
	parts = append(parts, ColorizeStatus(e.State, e.Detail))
	if e.Rescue != nil {
		parts = append(parts, ColorizeRescue(e.Rescue, time.Now()))
	}

	_, _ = fmt.Fprintf(w, "\033[2K%s\n", strings.Join(parts, " "))
}

func UpdateTable(w *os.File, entries []StatusEntry, cols []TableColumn) {
	lineCount := len(entries) + 2

	_, _ = fmt.Fprintf(w, "\033[%dA", lineCount)

	PrintTableHeader(w, cols)

	for _, e := range entries {
		PrintRow(w, e, cols)
	}
}

// DisableLineWrap turns off the terminal's auto-wrap (DECAWM) so rows
// wider than the terminal are clipped at the right edge instead of
// wrapping onto a second physical line. UpdateTable's cursor-up math
// counts logical rows, so a wrapped row would desync the redraw and
// produce the garbled output users see when CI detail strings push a
// line past the terminal width.
func DisableLineWrap(w *os.File) {
	_, _ = fmt.Fprint(w, "\033[?7l")
}

// EnableLineWrap restores the terminal's auto-wrap mode. Always pair it
// with DisableLineWrap (typically via defer) so an early return does
// not leave the user's terminal in a clipped state.
func EnableLineWrap(w *os.File) {
	_, _ = fmt.Fprint(w, "\033[?7h")
}

func ColorizeStatus(state StatusState, detail string) string {
	label := state.String()
	if detail != "" {
		label = fmt.Sprintf("%s (%s)", label, detail)
	}

	switch state {
	case StatusMerged, StatusAlreadyMerged, StatusAutoMerge:
		return fmt.Sprintf("\033[32m%s\033[0m", label) // green
	case StatusFailedSecurity:
		// Bold + bright red so security failures pop out compared to the
		// regular red used for ordinary failures.
		return fmt.Sprintf("\033[1;91m%s\033[0m", label)
	case StatusFailed, StatusConflict, StatusUntrustedAuthor:
		return fmt.Sprintf("\033[31m%s\033[0m", label) // red
	case StatusBlockedCI, StatusNoVerdict:
		// Magenta so a budget block and a check that established nothing
		// both read as "not a code failure", distinct from the red used for
		// genuine failures.
		return fmt.Sprintf("\033[35m%s\033[0m", label)
	case StatusSkipped, StatusStale, StatusCancelled, StatusObsolete:
		// Yellow: not a real failure, but somebody has to act (a stale
		// branch wants a refresh, a cancelled build wants a retry, an
		// obsolete PR wants closing; the same color as a stale rescue
		// marker).
		return fmt.Sprintf("\033[33m%s\033[0m", label)
	case StatusChecking, StatusApproving, StatusMerging, StatusRefreshed, StatusRetried:
		// Cyan: work in progress -- a refreshed branch or a retried build
		// is re-running CI.
		return fmt.Sprintf("\033[36m%s\033[0m", label)
	default:
		return label
	}
}

func PrintPlainResults(w *os.File, status *PRStatus) {
	merged := status.MergedEntries()
	autoMerge := status.AutoMergeEntries()
	securityFailed, otherFailed := SplitActionRequired(status.ActionRequired())
	blocked := status.BlockedEntries()
	skipped := status.SkippedEntries()

	if len(merged) > 0 {
		_, _ = fmt.Fprintf(w, "Merged (%d):\n", len(merged))
		for _, e := range merged {
			printPlainEntry(w, e)
		}
	}

	if len(autoMerge) > 0 {
		_, _ = fmt.Fprintf(w, "Left to auto-merge (%d):\n", len(autoMerge))
		for _, e := range autoMerge {
			printPlainEntry(w, e)
		}
		_, _ = fmt.Fprintln(w)
	}

	printFailureGroup(w, "Security failures", securityFailed)
	printFailureGroup(w, "Failed", otherFailed)
	printFailureGroup(w, StaleGroupHeader, status.StaleEntries())
	printFailureGroup(w, RefreshedGroupHeader, status.RefreshedEntries())
	printFailureGroup(w, CancelledGroupHeader, status.CancelledEntries())
	printFailureGroup(w, RetriedGroupHeader, status.RetriedEntries())
	printFailureGroup(w, ObsoleteGroupHeader, status.ObsoleteEntries())
	printFailureGroup(w, "CI unavailable (Actions budget)", blocked)
	printFailureGroup(w, NoVerdictGroupHeader, status.NoVerdictEntries())

	if len(skipped) > 0 {
		_, _ = fmt.Fprintf(w, "Skipped (%d):\n", len(skipped))
		for _, e := range skipped {
			printPlainEntry(w, e)
		}
		_, _ = fmt.Fprintln(w)
	}
}

// Section headers shared by the plain-text and TUI summaries for the stale
// and cancelled buckets, so both outputs name them identically.
const (
	StaleGroupHeader     = "Stale (behind base, failing checks green there -- refresh first)"
	RefreshedGroupHeader = "Refreshed (re-checking)"
	CancelledGroupHeader = "Cancelled (CircleCI auto-cancelled the build -- retry first)"
	RetriedGroupHeader   = "Retried (re-checking)"
	// NoVerdictGroupHeader names the bucket for checks that established
	// nothing about the code. Each entry's detail names its own remedy.
	NoVerdictGroupHeader = "CI unavailable (no verdict) -- the checks decided nothing, see the detail"
	// ObsoleteGroupHeader names the bucket for bot PRs that are not worth
	// fixing. Each entry's detail says whether a higher-version sibling
	// replaced it or its diff changes nothing that executes.
	ObsoleteGroupHeader = "Obsolete (nothing to fix -- close these)"
)

// printFailureGroup writes a header and one entry per failure including its
// PR URL on a continuation line. It is a no-op when entries is empty.
func printFailureGroup(w *os.File, header string, entries []StatusEntry) {
	if len(entries) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "%s (%d):\n", header, len(entries))
	for _, e := range entries {
		printPlainEntry(w, e)
		_, _ = fmt.Fprintf(w, "         %s\n", e.PR.URL)
	}
	_, _ = fmt.Fprintln(w)
}

func printPlainEntry(w *os.File, e StatusEntry) {
	detail := e.State.String()
	if e.Detail != "" {
		detail = fmt.Sprintf("%s (%s)", detail, e.Detail)
	}
	dep := ExtractDependencyName(e.PR.Title)
	ver := ExtractVersion(e.PR.Title)
	if dep == "" {
		dep = e.PR.Title
	}
	verStr := ""
	if ver != "" {
		verStr = " " + ver
	}
	ageStr := ""
	if age := FormatAge(e.PR.CreatedAt, time.Now()); age != "" {
		ageStr = fmt.Sprintf(" [%s old]", age)
	}
	rescueStr := ""
	if e.Rescue != nil {
		rescueStr = fmt.Sprintf(" [%s]", FormatRescue(e.Rescue, time.Now()))
	}
	_, _ = fmt.Fprintf(w, "  #%-6d %s/%s  %s%s%s  [%s] [%s]%s\n",
		e.PR.Number, e.PR.Owner, e.PR.Repo, dep, verStr, ageStr, e.PR.Author, detail, rescueStr)
}

// AdjustColumnWidths widens each column so every value fits without truncation.
func AdjustColumnWidths(cols []TableColumn, prs []PRInfo) {
	for i, c := range cols {
		w := len(c.Label)
		for _, p := range prs {
			if v := len(c.Fn(p)); v > w {
				w = v
			}
		}
		cols[i].Width = w
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
