package pr

import (
	"regexp"
	"strings"

	"github.com/google/go-github/v92/github"
)

// chartVersionRE matches a Chart.yaml version line: the chart's own
// `version:` and `appVersion:`, and each dependency's `version:`.
var chartVersionRE = regexp.MustCompile(`^\s*(?:-\s*)?(?:version|appVersion):\s*["']?([^"'\s#]+)`)

// ClassifyChartSync derives the update type of an upstream sync PR from its
// diff. The PR body names only the versions synced to, so the source
// versions come from the Chart.yaml lines the sync rewrote: within each
// Chart.yaml the removed and added version lines are paired by order, and
// the PR takes the largest change of its pairs.
//
// Anything that cannot be read is UpdateUnknown, never a guess: no file, a
// Chart.yaml patch GitHub did not deliver, an unequal number of removed and
// added version lines, or no version line changed at all.
func ClassifyChartSync(files []*github.CommitFile) UpdateType {
	largest := UpdateUnknown
	for _, f := range files {
		if f.GetFilename() != "Chart.yaml" && !strings.HasSuffix(f.GetFilename(), "/Chart.yaml") {
			continue
		}
		if f.GetPatch() == "" {
			return UpdateUnknown
		}
		removed, added := changedLines(f.GetPatch())
		from, to := chartVersions(removed), chartVersions(added)
		if len(from) != len(to) {
			return UpdateUnknown
		}
		for i := range from {
			change := diffType(from[i], to[i])
			if change == UpdateUnknown {
				return UpdateUnknown
			}
			largest = larger(largest, change)
		}
	}
	return largest
}

// chartVersions returns the versions named on the Chart.yaml version lines
// among lines, in order.
func chartVersions(lines []string) []string {
	var out []string
	for _, line := range lines {
		if m := chartVersionRE.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}
