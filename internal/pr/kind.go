package pr

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Kind is the bot that authored a PR. The sweep touches PRs of these four
// kinds and nothing else; a human-authored PR has no kind.
type Kind string

const (
	KindRenovate   Kind = "renovate"
	KindAlignFiles Kind = "align-files"
	KindHerald     Kind = "herald"
	KindDependabot Kind = "dependabot"
)

// kindByLogin maps the GitHub login of each trusted bot to its kind.
var kindByLogin = map[string]Kind{
	"renovate[bot]":               KindRenovate,
	"giantswarm-align-files[bot]": KindAlignFiles,
	"heraldbot[bot]":              KindHerald,
	"dependabot[bot]":             KindDependabot,
}

// KindOf returns the kind of the bot with the given login, or "" when the
// login is not one of the four trusted bots.
func KindOf(login string) Kind {
	return kindByLogin[login]
}

// TrustedLogins returns the logins of the four trusted bots, sorted.
func TrustedLogins() []string {
	out := make([]string, 0, len(kindByLogin))
	for login := range kindByLogin {
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}

// UpdateType is the semantic size of a dependency update. Align files and
// Herald PRs carry no version change and are UpdateNone; a Renovate or
// Dependabot PR whose versions cannot be read is UpdateUnknown.
type UpdateType string

const (
	UpdateMajor    UpdateType = "major"
	UpdateMinor    UpdateType = "minor"
	UpdatePatch    UpdateType = "patch"
	UpdateDigest   UpdateType = "digest"
	UpdatePin      UpdateType = "pin"
	UpdateLockfile UpdateType = "lockfile"
	UpdateNone     UpdateType = "none"
	UpdateUnknown  UpdateType = "unknown"
)

var (
	groupedTypeRE = regexp.MustCompile(`(?i)\((major|minor|patch|digest)\)\s*$`)
	// changeCellRE matches one "Change" cell of the Renovate PR body table:
	// two backticked versions joined by an arrow.
	changeCellRE = regexp.MustCompile("`([^`]+)`\\s*(?:→|->)\\s*`([^`]+)`")
	// dependabotUpdateRE matches one "Updates `dep` from X to Y" line of a
	// Dependabot group PR body.
	dependabotUpdateRE = regexp.MustCompile("(?m)^Updates `[^`]+` from (\\S+) to (\\S+)")
	// goMajorPathRE matches the /vN suffix of a Go module path.
	goMajorPathRE = regexp.MustCompile(`/v(\d+)$`)
	hexRE         = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

// ClassifyUpdate derives the update type of a bot PR from what the bot
// wrote. Dependabot titles name both versions ("from X to Y"); a Dependabot
// group names them per dependency in the body. Renovate titles name only
// the target, so the source comes from the body's Change column. Grouped
// PRs take the largest change of their rows. Anything that cannot be read
// is UpdateUnknown, never a guess.
func ClassifyUpdate(kind Kind, title, body string) UpdateType {
	switch kind {
	case KindAlignFiles, KindHerald:
		return UpdateNone
	}

	bare := strings.ToLower(stripConventionalCommitPrefix(strings.TrimSpace(title)))
	switch {
	case strings.HasPrefix(bare, "lock file maintenance"):
		return UpdateLockfile
	case strings.HasPrefix(bare, "pin dependenc"):
		return UpdatePin
	case strings.HasPrefix(bare, "update all non-major"):
		return UpdateMinor
	}
	if m := groupedTypeRE.FindStringSubmatch(bare); m != nil {
		return UpdateType(m[1])
	}

	if versions := versionMatch(title); len(versions) == 2 {
		return diffType(versions[0], versions[1])
	}

	largest := UpdateUnknown
	for _, m := range changeCellRE.FindAllStringSubmatch(body, -1) {
		largest = larger(largest, diffType(m[1], m[2]))
	}
	for _, m := range dependabotUpdateRE.FindAllStringSubmatch(body, -1) {
		largest = larger(largest, diffType(m[1], m[2]))
	}
	if largest != UpdateUnknown {
		return largest
	}

	// A Go major bump renames the module path: "update module foo/v91 to v92".
	if dep := ExtractDependencyName(title); dep != "" {
		if from := goMajorPathRE.FindStringSubmatch(dep); from != nil {
			if to, err := semver.NewVersion(ExtractTargetVersion(title)); err == nil {
				if from[1] != strconv.FormatUint(to.Major(), 10) {
					return UpdateMajor
				}
			}
		}
	}
	return UpdateUnknown
}

// diffType compares two version strings. Two hex digests are a digest
// update; a range operator in front of a version (npm's ~ and ^, a bare =)
// is dropped; anything semver cannot read is unknown.
func diffType(from, to string) UpdateType {
	from, to = trimRange(from), trimRange(to)
	if hexRE.MatchString(from) && hexRE.MatchString(to) {
		return UpdateDigest
	}
	a, errA := semver.NewVersion(from)
	b, errB := semver.NewVersion(to)
	if errA != nil || errB != nil {
		return UpdateUnknown
	}
	switch {
	case a.Major() != b.Major():
		return UpdateMajor
	case a.Minor() != b.Minor():
		return UpdateMinor
	default:
		return UpdatePatch
	}
}

func trimRange(v string) string {
	return strings.TrimLeft(strings.TrimSpace(v), "~^=")
}

// updateRank orders update types by size so a grouped PR takes its largest.
var updateRank = map[UpdateType]int{
	UpdateUnknown: 0,
	UpdateDigest:  1,
	UpdatePatch:   2,
	UpdateMinor:   3,
	UpdateMajor:   4,
}

func larger(a, b UpdateType) UpdateType {
	if updateRank[b] > updateRank[a] {
		return b
	}
	return a
}
