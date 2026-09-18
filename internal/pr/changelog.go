package pr

import (
	"bufio"
	"fmt"
	"strings"
	"text/template"
)

// ParseChangelogTemplate compiles one team's changelog line. A template that
// names a value ChangelogFacts does not carry is an error, so a team reads
// back the line it wrote rather than an empty one.
func ParseChangelogTemplate(text string) (*template.Template, error) {
	return template.New("changelog").Option("missingkey=error").Parse(text)
}

// ChangelogLine renders the entry one PR earns under a policy.
func ChangelogLine(policy ChangelogPolicy, facts ChangelogFacts) (string, error) {
	parsed, err := ParseChangelogTemplate(policy.Template)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	if err := parsed.Execute(&out, facts); err != nil {
		return "", err
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		return "", fmt.Errorf("the changelog template rendered an empty line")
	}
	return line, nil
}

// InsertChangelogLine returns content with line added under the policy's
// heading and section, and reports whether it changed anything. A file that
// already carries the line is returned untouched, so asking twice writes
// once.
//
// The section is created under the heading when the file does not carry it,
// and the heading is created at the top of the file, under its title, when
// the file does not carry that either. The entry goes at the end of the
// section's list, where a person writing by hand puts it.
func InsertChangelogLine(content string, policy ChangelogPolicy, line string) (string, bool) {
	lines := splitLines(content)
	if containsLine(lines, line) {
		return content, false
	}

	heading := strings.TrimSpace(policy.Heading)
	section := strings.TrimSpace(policy.Section)

	headingAt := indexOfHeading(lines, heading)
	if headingAt < 0 {
		return joinLines(insertAt(lines, titleEnd(lines), []string{heading, "", section, "", line, ""})), true
	}

	end := endOfSection(lines, headingAt+1, headingLevel(heading))
	sectionAt := indexOfHeadingIn(lines, section, headingAt+1, end)
	if sectionAt < 0 {
		return joinLines(insertAt(lines, headingAt+1, []string{"", section, "", line})), true
	}

	return joinLines(insertAt(lines, endOfEntries(lines, sectionAt+1, end), []string{line})), true
}

// splitLines keeps the file's lines without its trailing newline, which
// joinLines puts back: a changelog ends with one newline, whatever the
// insertion did.
func splitLines(content string) []string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

func containsLine(lines []string, line string) bool {
	for _, have := range lines {
		if strings.TrimSpace(have) == strings.TrimSpace(line) {
			return true
		}
	}
	return false
}

func insertAt(lines []string, at int, added []string) []string {
	at = min(max(at, 0), len(lines))
	out := make([]string, 0, len(lines)+len(added))
	out = append(out, lines[:at]...)
	out = append(out, added...)
	return append(out, lines[at:]...)
}

// headingLevel is the number of leading hashes, and 0 for a line that is no
// heading.
func headingLevel(line string) int {
	trimmed := strings.TrimSpace(line)
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level >= len(trimmed) || trimmed[level] != ' ' {
		return 0
	}
	return level
}

func indexOfHeading(lines []string, heading string) int {
	return indexOfHeadingIn(lines, heading, 0, len(lines))
}

// indexOfHeadingIn finds a heading between from and to, comparing the text
// alone: a team writes "## [Unreleased]" and the file may carry
// "## [Unreleased] - 2026-09-18".
func indexOfHeadingIn(lines []string, heading string, from, to int) int {
	wanted := strings.TrimSpace(heading)
	for index := max(from, 0); index < min(to, len(lines)); index++ {
		line := strings.TrimSpace(lines[index])
		if line == wanted || strings.HasPrefix(line, wanted+" ") {
			return index
		}
	}
	return -1
}

// endOfSection is the first line at or above the given heading level, which
// is where the heading's own content ends.
func endOfSection(lines []string, from, level int) int {
	for index := from; index < len(lines); index++ {
		if got := headingLevel(lines[index]); got > 0 && got <= level {
			return index
		}
	}
	return len(lines)
}

// endOfEntries is the line after the last entry of a section: the blank
// lines that trail it belong to whatever follows.
func endOfEntries(lines []string, from, to int) int {
	end := from
	for index := from; index < min(to, len(lines)); index++ {
		if headingLevel(lines[index]) > 0 {
			break
		}
		if strings.TrimSpace(lines[index]) != "" {
			end = index + 1
		}
	}
	return end
}

// titleEnd is where a new heading goes when the file carries none: after the
// file's title and the paragraph under it, or at the top of a file with no
// title.
func titleEnd(lines []string) int {
	title := -1
	for index, line := range lines {
		if headingLevel(line) == 1 {
			title = index
			break
		}
	}
	if title < 0 {
		return 0
	}
	for index := title + 1; index < len(lines); index++ {
		if headingLevel(lines[index]) > 0 {
			return index
		}
	}
	return len(lines)
}
