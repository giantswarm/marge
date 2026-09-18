package pr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ExtractOwnerRepo parses owner and repo from a GitHub HTML URL
// (e.g. "https://github.com/OWNER/REPO/pull/123").
func ExtractOwnerRepo(htmlURL string) (string, string, error) {
	parts := strings.Split(strings.TrimPrefix(htmlURL, "https://github.com/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("unexpected URL format: %s", htmlURL)
	}
	return parts[0], parts[1], nil
}

// ParsePRURL parses owner, repo, and PR number from a GitHub pull request
// URL (e.g. "https://github.com/OWNER/REPO/pull/123").
func ParsePRURL(htmlURL string) (owner, repo string, number int, err error) {
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(htmlURL, "https://github.com/"), "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("not a pull request URL: %s", htmlURL)
	}
	number, err = strconv.Atoi(parts[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid PR number in URL %s: %w", htmlURL, err)
	}
	return parts[0], parts[1], number, nil
}

var conventionalCommitPrefixRE = regexp.MustCompile(`(?i)^\w+(\([^)]*\))?:\s*`)

func stripConventionalCommitPrefix(title string) string {
	return conventionalCommitPrefixRE.ReplaceAllString(title, "")
}

var dependencyPatterns = []*regexp.Regexp{
	// Renovate: "Update dependency foo/bar to v1.2.3"
	regexp.MustCompile(`(?i)update dependency ([@\w\-./]+(?:/[@\w\-./]+)*)`),
	// Renovate: "Update module foo/bar to v1.2.3"
	regexp.MustCompile(`(?i)update module ([@\w\-./]+(?:/[@\w\-./]+)*)`),
	// Renovate: "Update github-actions action foo/bar to v1.2.3"
	regexp.MustCompile(`(?i)update [\w\-]+ action ([@\w\-./]+(?:/[@\w\-./]+)*)`),
	// Renovate: "Update actions/checkout action to v5.1.0" -- the action
	// names itself, with no manager word in front.
	regexp.MustCompile(`(?i)^update ([@\w\-./]+(?:/[@\w\-./]+)*) action\b`),
	// Renovate: "Update gsoci.azurecr.io/giantswarm/pause docker tag to
	// v3.10.2", and the digest, orb and image shapes beside it -- the
	// dependency names itself and the manager word follows it.
	regexp.MustCompile(`(?i)^update ([@\w\-./]+(?:/[@\w\-./]+)*) (?:docker tag|digest|orb|image)\b`),
	// Renovate: "Update rust crate kube to v3"
	regexp.MustCompile(`(?i)update [\w\-]+ crate ([@\w\-./]+(?:/[@\w\-./]+)*)`),
	// Renovate: "Update terraform aws to v5"
	regexp.MustCompile(`(?i)update terraform ([@\w\-./]+(?:/[@\w\-./]+)*)`),
	// Renovate: "Update opentelemetry-go monorepo" or "Update eslint monorepo to v10"
	regexp.MustCompile(`(?i)update ([@\w\-./]+(?:/[@\w\-./]+)*) monorepo`),
	// Renovate: "Update all non-major dependencies"
	regexp.MustCompile(`(?i)update (all [\w\-]+ dependencies)`),
	// Renovate: "Pin dependency foo to v1.2.3"
	regexp.MustCompile(`(?i)pin dependency ([@\w\-./]+(?:/[@\w\-./]+)*)`),
	// Renovate: "Replace dependency foo with bar"
	regexp.MustCompile(`(?i)replace dependency ([@\w\-./]+(?:/[@\w\-./]+)*)`),
	// Renovate: "Lock file maintenance"
	regexp.MustCompile(`(?i)^(lock file maintenance)$`),
	// Renovate: "Update foo/bar to v1.2.3" (generic fallback, must be after specific patterns)
	regexp.MustCompile(`(?i)^update ([@\w\-./]+(?:/[@\w\-./]+)*) to `),
	// Renovate: grouped update "Update github-actions (major)" or "Update npm (minor)"
	regexp.MustCompile(`(?i)^update ([@\w\-./]+(?:/[@\w\-./]+)*)\s+\((?:major|minor|patch|digest)\)\s*$`),
	// Dependabot: "Bump foo from 1.2.3 to 1.2.4"
	regexp.MustCompile(`(?i)bump ([@\w\-./]+(?:/[@\w\-./]+)*) from`),
	// Dependabot: "Bump the foo group ..."
	regexp.MustCompile(`(?i)bump the ([\w\-]+) group`),
	// Dependabot: "Bump foo to 1.2.3"
	regexp.MustCompile(`(?i)bump ([@\w\-./]+(?:/[@\w\-./]+)*) to `),
	// Renovate onboarding: "Configure Renovate"
	regexp.MustCompile(`(?i)^(configure renovate)$`),
	// Dependabot onboarding: "Set package ecosystem to 'gomod' in dependabot config"
	regexp.MustCompile(`(?i)set package[- ]ecosystem to '?([\w\-]+)'?`),
}

// IsDependencyUpdateTitle returns true if the PR title looks like an automated
// dependency update (Renovate or Dependabot), regardless of who authored it.
func IsDependencyUpdateTitle(title string) bool {
	title = strings.TrimSpace(title)
	lower := strings.ToLower(title)

	// Conventional commit with deps scope: "chore(deps):", "fix(deps):", etc.
	if scope, _, ok := strings.Cut(lower, ":"); ok && strings.Contains(scope, "deps") {
		return true
	}

	// Known dependency update title patterns (Renovate/Dependabot)
	return ExtractDependencyName(title) != ""
}

var versionPatterns = []*regexp.Regexp{
	// "from vX.Y.Z to vA.B.C" – captures both source and target
	regexp.MustCompile(`(?i)\bfrom\s+(v?\d[\w.\-+]*)\s+to\s+(v?\d[\w.\-+]*)`),
	// "to vX.Y.Z" at end of title (with optional parenthetical like "(major)")
	regexp.MustCompile(`(?i)\bto\s+(v?\d[\w.\-+]*)(?:\s*\(.*\))?\s*$`),
}

// versionMatch returns the versions named in a dependency-update title
// (without its conventional-commit prefix): [source, target] for
// "from X to Y", [target] for "to Y", nil when the title names none.
func versionMatch(title string) []string {
	title = stripConventionalCommitPrefix(strings.TrimSpace(title))
	for _, pat := range versionPatterns {
		if m := pat.FindStringSubmatch(title); len(m) >= 2 {
			return m[1:]
		}
	}
	return nil
}

// ExtractVersion renders the version change named in a title for display:
// "4.17.20 -> 4.17.21" for a from/to title, "v1.10.2" for a to-only title,
// "" when the title names no version.
func ExtractVersion(title string) string {
	switch m := versionMatch(title); len(m) {
	case 0:
		return ""
	case 1:
		return m[0]
	default:
		return m[0] + " -> " + m[1]
	}
}

// ExtractVersions returns the versions named in a dependency-update title as
// a pair: the version the update moves from, empty when the title names only
// the target, and the version it moves to.
func ExtractVersions(title string) (from, to string) {
	switch m := versionMatch(title); len(m) {
	case 0:
		return "", ""
	case 1:
		return "", m[0]
	default:
		return m[0], m[1]
	}
}

// ExtractTargetVersion returns the version a dependency update moves to,
// regardless of whether the title also names the version it moves from.
func ExtractTargetVersion(title string) string {
	m := versionMatch(title)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

func ExtractDependencyName(title string) string {
	title = strings.TrimSpace(title)
	title = stripConventionalCommitPrefix(title)
	for _, pat := range dependencyPatterns {
		matches := pat.FindStringSubmatch(title)
		if len(matches) >= 2 {
			return matches[1]
		}
	}
	return ""
}

// ParsePRRef reads one pull request reference in either spelling a caller
// uses: the browser URL, or the "owner/repo#number" shorthand that reads
// the way a sweep reports a PR.
func ParsePRRef(ref string) (owner, repo string, number int, err error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://") {
		return ParsePRURL(ref)
	}
	repoPart, numberPart, found := strings.Cut(ref, "#")
	if !found {
		return "", "", 0, fmt.Errorf("not a pull request reference: %s (want an URL or owner/repo#number)", ref)
	}
	owner, repo, found = strings.Cut(repoPart, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", 0, fmt.Errorf("not a pull request reference: %s (want an URL or owner/repo#number)", ref)
	}
	number, err = strconv.Atoi(numberPart)
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("invalid PR number in %s", ref)
	}
	return owner, repo, number, nil
}

// Key is how one PR is named in a selection: "owner/repo#number", with the
// repository lowercased because GitHub compares owner and repository names
// case-insensitively.
func Key(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", strings.ToLower(owner), strings.ToLower(repo), number)
}
