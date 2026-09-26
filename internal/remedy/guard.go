package remedy

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/giantswarm/marge/internal/pr"
)

// protectionSettleDelay is how long the head's checks must have been quiet
// before a required context nobody reported counts as one no job posts any
// more. A workflow that has not created its check run yet reports nothing,
// which is the same observation.
const protectionSettleDelay = 30 * time.Minute

// Guard reports why an action must not run, or "" when it may. Guards are
// pure: they read the Request and nothing else, so every one of them is
// exercised without a GitHub call. The name is what a report prints, so the
// set an action enforces is readable rather than buried in its code.
type Guard struct {
	Name   string
	Refuse func(*Request) string
}

// TrustedAuthor refuses a PR no trusted bot authored.
var TrustedAuthor = Guard{"trusted-author", func(req *Request) string {
	if pr.KindOf(req.Pull.GetUser().GetLogin(), pr.LabelNames(req.Pull.Labels)) == "" {
		return fmt.Sprintf("author %q is not a trusted bot", req.Pull.GetUser().GetLogin())
	}
	return ""
}}

// NoSecurityFailure refuses every action on a PR with a failing security
// check. A red security scan is never merged past, never retried and never
// rescued.
var NoSecurityFailure = Guard{"no-security-failure", func(req *Request) string {
	if req.SecurityFailure != "" {
		return "security check failed: " + req.SecurityFailure
	}
	return ""
}}

// RequiredChecksGreen refuses while a required context is failed, pending or
// missing. Missing is a wait: a context nobody reported may report later, and
// never reporting is not a pass either.
var RequiredChecksGreen = Guard{"required-checks-green", func(req *Request) string {
	switch {
	case len(req.Required.Failed) > 0:
		return "required checks failed: " + strings.Join(req.Required.Failed, ", ")
	case len(req.Required.Pending) > 0:
		return "required checks pending: " + strings.Join(req.Required.Pending, ", ")
	case len(req.Required.Missing) > 0:
		return "required checks not reported: " + strings.Join(req.Required.Missing, ", ")
	}
	return ""
}}

// LogMatched refuses an action the rule selected from a check name alone. A
// check name, a repository or a previous sweep's table is not evidence of
// what failed; the failing step's log is.
var LogMatched = Guard{"log-matched", func(req *Request) string {
	if !req.LogMatched {
		return "no log excerpt matched: a check name alone does not classify a failure"
	}
	return ""
}}

// OncePerChange refuses a second attempt of the same action against the
// change currently on the branch. Twice red on the same code is real.
func OncePerChange(name Name) Guard {
	return Guard{"once-per-change", func(req *Request) string {
		if req.AppliedThisChange[name] {
			return fmt.Sprintf("%s already applied to this change", name)
		}
		return ""
	}}
}

// knownPipelines are the e2e suites a comment may start. The set is closed
// and lives in Go, so a rule merged into the branch the sweep reads cannot
// widen it.
var knownPipelines = map[string]bool{
	"app-test-suites-single": true,
	"cluster-test-suites":    true,
	"releases-test-suites":   true,
}

// commandRE is the shape of a command an action may comment: a slash
// command with KEY=value arguments and nothing else.
var commandRE = regexp.MustCompile(`^/run ([a-z][a-z0-9-]*)((?: [A-Z][A-Z0-9_]*=[A-Za-z0-9,._-]+)*)$`)

// KnownCommand refuses a command the sweep did not recognise. The command
// is captured from a check's own message, so this is the fence that says
// which commands marge is allowed to have been told; it is not a choice
// between them, which stays with the check that named one.
var KnownCommand = Guard{"known-command", func(req *Request) string {
	if len(req.Commands) == 0 {
		return "the rule captured no command"
	}
	for _, command := range req.Commands {
		match := commandRE.FindStringSubmatch(command)
		if match == nil {
			return fmt.Sprintf("command %q is not a /run command with KEY=value arguments", command)
		}
		if !knownPipelines[match[1]] {
			return fmt.Sprintf("pipeline %q is not one the sweep starts", match[1])
		}
	}
	return ""
}}

// OnlyGateWaiting refuses while anything other than the matched check keeps
// the PR from merging. A comment starts a real test suite on real
// infrastructure, and a PR whose other checks are red does not merge when
// that suite goes green.
var OnlyGateWaiting = Guard{"only-gate-waiting", func(req *Request) string {
	switch {
	case len(req.Required.Failed) > 0:
		return "required checks failed: " + strings.Join(req.Required.Failed, ", ")
	case len(req.Required.Missing) > 0:
		return "required checks not reported: " + strings.Join(req.Required.Missing, ", ")
	case len(req.Failing) > 0:
		return "checks failed: " + strings.Join(req.Failing, ", ")
	}
	for _, name := range req.Required.Pending {
		if name != req.Check {
			return "required checks pending besides " + req.Check + ": " + name
		}
	}
	return ""
}}

// ChecksSettled refuses while the head may still report a context for the
// first time. A context nobody reported is drift only once the head has
// reported something, nothing is running, and the newest report is older
// than protectionSettleDelay.
var ChecksSettled = Guard{"checks-settled", func(req *Request) string {
	switch {
	case req.Reported == 0:
		return "the head has reported no check yet"
	case len(req.Required.Pending) > 0:
		return "required checks pending: " + strings.Join(req.Required.Pending, ", ")
	case req.ChecksPending:
		return "a check on the head has not finished"
	case req.ChecksSettledAt.IsZero():
		return "the head's checks carry no completion time"
	}
	if quiet := req.Now.Sub(req.ChecksSettledAt); quiet < protectionSettleDelay {
		return fmt.Sprintf("the head's checks settled %s ago, less than %s: a context may still report for the first time",
			quiet.Round(time.Minute), protectionSettleDelay)
	}
	return ""
}}

// NoGeneratedEdit refuses an action that would write a file rendered by a
// generator. The remedy for such a file is a change to its generator.
var NoGeneratedEdit = Guard{"no-generated-edit", func(req *Request) string {
	for _, p := range req.Writes {
		if reason := generatedBy(p); reason != "" {
			return fmt.Sprintf("%s is generated by %s: change the generator, not the file", p, reason)
		}
	}
	return ""
}}

// The two generators that render a file no remedy may hand-edit.
const (
	byDevctl   = "devctl"
	byTemplate = "giantswarm/github"
)

// generatedPaths maps an exact repository path to the generator that renders
// it. A path absent from both this map and the zz_ prefix is repository
// owned and may be written.
var generatedPaths = map[string]string{
	"renovate.json5":          byDevctl,
	".pre-commit-config.yaml": byDevctl,
	".circleci/config.yml":    byDevctl,
	".circleci/workflows.yml": byDevctl,
	"CODEOWNERS":              byTemplate,
	"Makefile.gen.go.mk":      byDevctl,
	"Makefile.gen.app.mk":     byDevctl,
}

// zzPrefix marks every file rendered from a giantswarm/github template. It
// appears as a file-name prefix at any depth.
const zzPrefix = "zz_"

// generatedBy names the generator of a path, or "" when the path is
// repository owned.
func generatedBy(p string) string {
	clean := path.Clean(strings.TrimPrefix(p, "./"))
	if by, ok := generatedPaths[clean]; ok {
		return by
	}
	if strings.HasPrefix(path.Base(clean), zzPrefix) {
		return byTemplate
	}
	return ""
}
