// Package rules holds the declarative catalogue the sweep loads at runtime.
// A rule names a detection signal, one action of the closed remedy
// vocabulary and the evidence written on the PR. Guards live in the action,
// so a rule can add a refusal and never remove one.
package rules

import (
	"regexp"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
)

// maxLogExcerpt bounds the log an action fetches for one check. A signal
// reads the tail of the failing step's output, never the whole build.
const (
	defaultLogExcerpt = 64 << 10
	maxLogExcerpt     = 1 << 20
)

// Rule is one document of the catalogue.
type Rule struct {
	// Name identifies the rule and matches its file name.
	Name string `yaml:"name"`
	// Summary says in one line what the rule recognises.
	Summary string `yaml:"summary"`
	// Source cites where the pattern comes from, for instance the runbook
	// row it was imported from.
	Source string `yaml:"source"`

	Match    Match    `yaml:"match"`
	Action   Action   `yaml:"action"`
	Refuse   []string `yaml:"refuse"`
	Evidence Evidence `yaml:"evidence"`

	compiled compiled
}

// Match is the detection signal. Every field present must hold; a rule with
// no signal beyond states and kinds is refused, because it would match every
// PR in that classification.
type Match struct {
	// States names the classifications the rule applies to, from the
	// remediable set. Empty applies to every remediable state.
	States []string `yaml:"states"`
	// Kinds names the bot PR kinds. Empty applies to every trusted kind.
	Kinds []string `yaml:"kinds"`

	Check *CheckMatch `yaml:"check"`
	Log   *LogMatch   `yaml:"log"`
	PR    *PRMatch    `yaml:"pr"`
}

// CheckMatch matches the name of a failing check. A check name alone never
// justifies a write, so a rule whose action writes also needs a log signal.
type CheckMatch struct {
	// Name is a glob over the failing check names.
	Name string `yaml:"name"`
}

// LogMatch matches an excerpt of the failing step's log.
type LogMatch struct {
	// Source selects where the excerpt comes from.
	Source LogSource `yaml:"source"`
	// Pattern is an RE2 expression matched against the excerpt.
	Pattern string `yaml:"pattern"`
	// MaxBytes bounds the excerpt read from the tail of the log. Zero uses
	// the default.
	MaxBytes int `yaml:"maxBytes"`
}

// LogSource names a log the sweep can fetch.
type LogSource string

const (
	LogActions  LogSource = "actions"
	LogCircleCI LogSource = "circleci"
)

// PRMatch matches metadata of the pull request itself.
type PRMatch struct {
	// BaseHead states what the base head must report for the failing checks.
	BaseHead BaseHead `yaml:"baseHead"`
	// TitlePattern is an RE2 expression matched against the PR title.
	TitlePattern string `yaml:"titlePattern"`
	// Files are globs; every one of them must match a file of the diff.
	Files []string `yaml:"files"`
}

// BaseHead is what the base branch head reported for the checks failing on
// the PR. Absent is its own value: a check the base never ran is not green.
type BaseHead string

const (
	BaseAny    BaseHead = ""
	BaseGreen  BaseHead = "green"
	BaseRed    BaseHead = "red"
	BaseAbsent BaseHead = "absent"
)

// Action names the remedy the rule selects.
type Action struct {
	Name remedy.Name `yaml:"name"`
}

// Evidence is the marker the sweep writes when the action applied. The
// marker's outcome is the action name, so a later sweep recognises what ran
// on this change whichever rule selected it.
type Evidence struct {
	// Reason is the line written on the PR under the action name.
	Reason string `yaml:"reason"`
}

// compiled holds what validation derived from the document, so matching
// compiles no expression and resolves no name.
type compiled struct {
	states  map[pr.StatusState]bool
	kinds   map[pr.Kind]bool
	checkRE *regexp.Regexp
	fileREs []*regexp.Regexp
	logRE   *regexp.Regexp
	titleRE *regexp.Regexp
	guards  []remedy.Guard
}

// States reports the classifications the rule applies to. An empty set in
// the document means every remediable state.
func (r *Rule) States() map[pr.StatusState]bool { return r.compiled.states }

// Kinds reports the bot PR kinds the rule applies to. An empty set in the
// document means every trusted kind.
func (r *Rule) Kinds() map[pr.Kind]bool { return r.compiled.kinds }

// CheckPattern returns the compiled check-name glob, or nil when the rule
// has no check signal.
func (r *Rule) CheckPattern() *regexp.Regexp { return r.compiled.checkRE }

// FilePatterns returns the compiled file globs. Every one of them must match
// a file of the diff.
func (r *Rule) FilePatterns() []*regexp.Regexp { return r.compiled.fileREs }

// LogPattern returns the compiled log expression, or nil when the rule has
// no log signal.
func (r *Rule) LogPattern() *regexp.Regexp { return r.compiled.logRE }

// TitlePattern returns the compiled title expression, or nil.
func (r *Rule) TitlePattern() *regexp.Regexp { return r.compiled.titleRE }

// Guards returns the refusals the rule adds to its action's own guards.
func (r *Rule) Guards() []remedy.Guard { return r.compiled.guards }

// LogBytes is the excerpt size the rule reads.
func (r *Rule) LogBytes() int {
	if r.Match.Log == nil || r.Match.Log.MaxBytes == 0 {
		return defaultLogExcerpt
	}
	return r.Match.Log.MaxBytes
}

// remediableStates are the classifications a rule may act on, by their name
// in a document. Merged, security and untrusted-author outcomes are absent:
// no rule acts on them.
var remediableStates = map[string]pr.StatusState{
	"failed":            pr.StatusFailed,
	"stale":             pr.StatusStale,
	"conflict":          pr.StatusConflict,
	"cancelled":         pr.StatusCancelled,
	"no-verdict":        pr.StatusNoVerdict,
	"blocked-ci":        pr.StatusBlockedCI,
	"obsolete":          pr.StatusObsolete,
	"waiting-checks":    pr.StatusWaitingChecks,
	"awaiting-approval": pr.StatusAwaitingApproval,
	"held":              pr.StatusHeld,
}

// refusals are the conditions a rule may add by name. Each resolves to a
// guard the action runs before it writes anything.
var refusals = map[string]func(remedy.Name) remedy.Guard{
	"log-matched":           func(remedy.Name) remedy.Guard { return remedy.LogMatched },
	"required-checks-green": func(remedy.Name) remedy.Guard { return remedy.RequiredChecksGreen },
	"no-security-failure":   func(remedy.Name) remedy.Guard { return remedy.NoSecurityFailure },
	"no-generated-edit":     func(remedy.Name) remedy.Guard { return remedy.NoGeneratedEdit },
	"once-per-change":       remedy.OncePerChange,
}

// Remediable reports whether a rule may act on a classification.
func Remediable(state pr.StatusState) bool {
	for _, remediable := range remediableStates {
		if remediable == state {
			return true
		}
	}
	return false
}
