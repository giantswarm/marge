package rules

import (
	"github.com/giantswarm/marge/internal/pr"
)

// CheckState is what the base branch head reported for one check name.
type CheckState string

const (
	CheckGreen  CheckState = "green"
	CheckRed    CheckState = "red"
	CheckAbsent CheckState = "absent"
)

// Subject is the classified PR a rule is matched against. The engine fills
// it once per PR; matching performs no request of its own except the log
// excerpt, which it asks Log for.
type Subject struct {
	State pr.StatusState
	Kind  pr.Kind
	Title string
	// Failing names the red checks that produced a verdict, in the order the
	// classification found them.
	Failing []string
	// BaseState says what the base head reported for each failing check.
	// A name absent from the map is CheckAbsent, and absent is not green.
	BaseState map[string]CheckState
	// MissingContexts names the required contexts of the base branch that
	// the head never reported.
	MissingContexts []string
	// Files returns the paths of the PR diff. It is called only for a rule
	// that carries a file signal, so a catalogue without one costs no
	// comparison. Nil returns no files, and such a rule does not match.
	Files func() []string
	// Log returns a bounded excerpt of one check's log. A rule with a log
	// signal never matches when Log is nil or reports false: an unreadable
	// log leaves the failure as it was classified.
	Log func(source LogSource, check string, maxBytes int) (string, bool)
}

// Hit is the rule that matched, with what made it match.
type Hit struct {
	Rule *Rule
	// Check is the failing check the signal matched, or "" for a rule whose
	// signal is PR metadata alone.
	Check string
	// Excerpt is the log excerpt the pattern matched, empty for a rule with
	// no log signal.
	Excerpt string
	// LogMatched reports whether a log excerpt decided the match. An action
	// that writes is guarded on it.
	LogMatched bool
	// MissingContexts are the required contexts the protection signal
	// selected. An action that rewrites a protection touches these and no
	// other.
	MissingContexts []string
}

// Match returns the first rule of the catalogue that matches the subject, or
// nil. The catalogue is ordered before it is used, so the outcome does not
// depend on how it was read; Catalogue.Rules states the order.
func (c *Catalogue) Match(subject *Subject) *Hit {
	if c == nil {
		return nil
	}
	for _, rule := range c.Rules {
		if hit := rule.match(subject); hit != nil {
			return hit
		}
	}
	return nil
}

func (r *Rule) match(subject *Subject) *Hit {
	if states := r.States(); len(states) > 0 && !states[subject.State] {
		return nil
	}
	if kinds := r.Kinds(); len(kinds) > 0 && !kinds[subject.Kind] {
		return nil
	}
	if !r.matchPRMetadata(subject) {
		return nil
	}
	missing, ok := r.matchProtection(subject)
	if !ok {
		return nil
	}

	candidates := r.candidates(subject)
	if r.Match.Check != nil && len(candidates) == 0 {
		return nil
	}
	if r.Match.PR != nil && r.Match.PR.BaseHead != BaseAny {
		if !allInBaseState(candidates, subject, CheckState(r.Match.PR.BaseHead)) {
			return nil
		}
	}

	if r.Match.Log == nil {
		return &Hit{Rule: r, Check: first(candidates), MissingContexts: missing}
	}
	hit := r.matchLog(subject, candidates)
	if hit != nil {
		hit.MissingContexts = missing
	}
	return hit
}

// matchProtection selects the required contexts the head never reported that
// any glob names. The globs are alternatives, because one migration renames
// job names of several shapes, and the selection is what the action
// rewrites: a context no glob named is left required. A rule whose globs
// select nothing does not match.
func (r *Rule) matchProtection(subject *Subject) ([]string, bool) {
	patterns := r.MissingContextPatterns()
	if len(patterns) == 0 {
		return nil, true
	}
	var selected []string
	for _, name := range subject.MissingContexts {
		for _, re := range patterns {
			if re.MatchString(name) {
				selected = append(selected, name)
				break
			}
		}
	}
	return selected, len(selected) > 0
}

// candidates are the failing checks the check signal selects. A rule without
// a check signal considers every failing check.
func (r *Rule) candidates(subject *Subject) []string {
	if r.Match.Check == nil {
		return subject.Failing
	}
	var out []string
	for _, name := range subject.Failing {
		if r.CheckPattern().MatchString(name) {
			out = append(out, name)
		}
	}
	return out
}

// allInBaseState reports whether the base head answers want for every check
// the rule selected. One of them is not enough: a PR that carries a
// transient failure the base fixed and a real failure beside it is not a PR
// the base head has anything to say about, and the evidence a rule writes
// speaks for every check it named.
func allInBaseState(candidates []string, subject *Subject, want CheckState) bool {
	if len(candidates) == 0 {
		return false
	}
	for _, name := range candidates {
		state, ok := subject.BaseState[name]
		if !ok {
			state = CheckAbsent
		}
		if state != want {
			return false
		}
	}
	return true
}

func (r *Rule) matchPRMetadata(subject *Subject) bool {
	p := r.Match.PR
	if p == nil {
		return true
	}
	if re := r.TitlePattern(); re != nil && !re.MatchString(subject.Title) {
		return false
	}
	patterns := r.FilePatterns()
	if len(patterns) == 0 {
		return true
	}
	if subject.Files == nil {
		return false
	}
	files := subject.Files()
	for _, re := range patterns {
		matched := false
		for _, file := range files {
			if re.MatchString(file) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// matchLog reads the excerpt of each candidate check and returns the first
// that carries the signal. A log that cannot be read is not a match.
func (r *Rule) matchLog(subject *Subject, candidates []string) *Hit {
	if subject.Log == nil {
		return nil
	}
	for _, name := range candidates {
		excerpt, ok := subject.Log(r.Match.Log.Source, name, r.LogBytes())
		if !ok {
			continue
		}
		if r.LogPattern().MatchString(excerpt) {
			return &Hit{Rule: r, Check: name, Excerpt: excerpt, LogMatched: true}
		}
	}
	return nil
}

func first(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
