package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
)

// nameRE is the shape of a rule name, which is also its file name.
var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Parse decodes and validates one rule document. Decoding is strict: an
// unknown key is an error, so no document can carry an option the schema
// does not define. fileName is the document's path, reported in errors and
// checked against the rule name.
func Parse(fileName string, data []byte, reg *remedy.Registry) (*Rule, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var rule Rule
	if err := dec.Decode(&rule); err != nil {
		return nil, fmt.Errorf("%s: %w", fileName, err)
	}
	if err := dec.Decode(new(Rule)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: one rule per file", fileName)
	}
	if err := rule.validate(fileName, reg); err != nil {
		return nil, fmt.Errorf("%s: %w", fileName, err)
	}
	return &rule, nil
}

func (r *Rule) validate(fileName string, reg *remedy.Registry) error {
	if !nameRE.MatchString(r.Name) {
		return fmt.Errorf("name %q must be lower-case words joined by hyphens", r.Name)
	}
	if base := strings.TrimSuffix(path.Base(fileName), path.Ext(fileName)); base != r.Name {
		return fmt.Errorf("name %q must match the file name %q", r.Name, base)
	}
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("summary is required")
	}
	if strings.TrimSpace(r.Source) == "" {
		return errors.New("source is required: cite where the pattern comes from")
	}
	if _, known := reg.Lookup(r.Action.Name); !known {
		return fmt.Errorf("unknown action %q: known actions are %s", r.Action.Name, joinNames(reg.Names()))
	}
	if err := r.validateMatch(); err != nil {
		return err
	}
	if err := r.validateRefusals(); err != nil {
		return err
	}
	if err := r.validateSignalStrength(); err != nil {
		return err
	}
	return r.validateEvidence()
}

// validateMatch refuses a rule with no signal of its own. States and kinds
// narrow a signal; they are not one, so a rule carrying only them would act
// on every PR of a classification.
func (r *Rule) validateMatch() error {
	if r.Match.Check == nil && r.Match.Log == nil && r.Match.PR == nil && r.Match.Protection == nil {
		return errors.New("match needs a check, log, pr or protection signal: states and kinds alone match every PR of a classification")
	}

	r.compiled.states = make(map[pr.StatusState]bool, len(r.Match.States))
	for _, name := range r.Match.States {
		state, ok := remediableStates[name]
		if !ok {
			return fmt.Errorf("unknown state %q: remediable states are %s", name, joinKeys(remediableStates))
		}
		r.compiled.states[state] = true
	}

	r.compiled.kinds = make(map[pr.Kind]bool, len(r.Match.Kinds))
	for _, name := range r.Match.Kinds {
		kind := pr.Kind(name)
		if !slices.Contains(trustedKinds, kind) {
			return fmt.Errorf("unknown kind %q: trusted kinds are %s", name, joinKinds(trustedKinds))
		}
		r.compiled.kinds[kind] = true
	}

	if c := r.Match.Check; c != nil {
		if err := checkGlob("match.check.name", c.Name); err != nil {
			return err
		}
		r.compiled.checkRE = compileCheckGlob(c.Name)
	}
	if l := r.Match.Log; l != nil {
		if l.Source != LogActions && l.Source != LogCircleCI {
			return fmt.Errorf("unknown log source %q: use %q or %q", l.Source, LogActions, LogCircleCI)
		}
		if strings.TrimSpace(l.Pattern) == "" {
			return errors.New("match.log.pattern is required when a log signal is given")
		}
		compiled, err := regexp.Compile(l.Pattern)
		if err != nil {
			return fmt.Errorf("match.log.pattern: %w", err)
		}
		if l.MaxBytes < 0 || l.MaxBytes > maxLogExcerpt {
			return fmt.Errorf("match.log.maxBytes %d is out of range: at most %d", l.MaxBytes, maxLogExcerpt)
		}
		r.compiled.logRE = compiled
	}
	if p := r.Match.PR; p != nil {
		switch p.BaseHead {
		case BaseAny, BaseGreen, BaseRed, BaseAbsent:
		default:
			return fmt.Errorf("unknown baseHead %q: use green, red or absent", p.BaseHead)
		}
		if p.TitlePattern != "" {
			compiled, err := regexp.Compile(p.TitlePattern)
			if err != nil {
				return fmt.Errorf("match.pr.titlePattern: %w", err)
			}
			r.compiled.titleRE = compiled
		}
		if p.BaseHead == BaseAny && p.TitlePattern == "" && len(p.Files) == 0 {
			return errors.New("match.pr needs baseHead, titlePattern or files")
		}
		for _, glob := range p.Files {
			if err := checkGlob("match.pr.files", glob); err != nil {
				return err
			}
			r.compiled.fileREs = append(r.compiled.fileREs, compilePathGlob(glob))
		}
	}
	if prot := r.Match.Protection; prot != nil {
		if len(prot.MissingContexts) == 0 {
			return errors.New("match.protection.missingContexts is required when a protection signal is given")
		}
		for _, glob := range prot.MissingContexts {
			if err := checkGlob("match.protection.missingContexts", glob); err != nil {
				return err
			}
			r.compiled.missingREs = append(r.compiled.missingREs, compileCheckGlob(glob))
		}
	}
	return nil
}

// checkGlob refuses an empty glob and one that matches everything. A glob of
// stars names no file and no context in particular, so it is a
// classification restated, not a signal, and validateSignalStrength would
// accept it as one.
func checkGlob(field, glob string) error {
	trimmed := strings.TrimSpace(glob)
	if trimmed == "" {
		return fmt.Errorf("%s holds an empty glob", field)
	}
	if strings.Trim(trimmed, "*/") == "" {
		return fmt.Errorf("%s: glob %q matches everything: name at least one literal segment", field, glob)
	}
	return nil
}

// validateRefusals resolves each named refusal to a guard. The names are a
// closed set, and a rule can only add to what its action already enforces.
func (r *Rule) validateRefusals() error {
	seen := make(map[string]bool, len(r.Refuse))
	for _, name := range r.Refuse {
		build, ok := refusals[name]
		if !ok {
			return fmt.Errorf("unknown refusal %q: known refusals are %s", name, joinKeys(refusals))
		}
		if seen[name] {
			return fmt.Errorf("refusal %q listed twice", name)
		}
		seen[name] = true
		r.compiled.guards = append(r.compiled.guards, build(r.Action.Name))
	}
	if seen[refusalLogMatched] && r.Match.Log == nil {
		return fmt.Errorf("refusal %q needs a log signal: a rule that reads no log never satisfies it, so the action would refuse every time", refusalLogMatched)
	}
	return nil
}

// validateSignalStrength refuses a rule that names no evidence of what
// failed. A check name says which job went red and a title says which
// dependency moved; neither says why, and the runbook is explicit that only
// the failing step's log classifies a failure. A title pattern is the
// weaker of the two, because "." reads every PR of a classification.
//
// A rule must therefore carry a log signal, or read the PR's state rather
// than its text: what the base head reported, which files the diff touches,
// or a required context nobody reported.
func (r *Rule) validateSignalStrength() error {
	if r.Match.Log != nil || r.hasStateSignal() {
		return nil
	}
	return errors.New("a check name or a title is not a diagnosis: add a log signal, or a baseHead, files or protection signal")
}

// hasStateSignal reports whether the rule reads something of the PR that a
// bot did not write, which is what makes a signal evidence rather than a
// restatement of the classification. Every glob it counts names at least
// one literal segment, so a glob of stars does not qualify as one.
func (r *Rule) hasStateSignal() bool {
	if r.Match.Protection != nil {
		return true
	}
	p := r.Match.PR
	if p == nil {
		return false
	}
	return p.BaseHead != BaseAny || len(p.Files) > 0
}

func (r *Rule) validateEvidence() error {
	if strings.TrimSpace(r.Evidence.Reason) == "" {
		return errors.New("evidence.reason is required: the PR must say why the action ran")
	}
	return nil
}

// trustedKinds are the bot PR kinds a rule may name.
var trustedKinds = []pr.Kind{pr.KindRenovate, pr.KindAlignFiles, pr.KindHerald, pr.KindDependabot}

func joinNames(names []remedy.Name) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return strings.Join(out, ", ")
}

func joinKinds(kinds []pr.Kind) string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}

func joinKeys[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// compileCheckGlob turns a check-name glob into an expression. A check name
// is not a path, so "*" stands for any run of characters. Matching ignores
// case, because the same job is named differently across repositories.
func compileCheckGlob(glob string) *regexp.Regexp {
	parts := strings.Split(glob, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile("(?i)^" + strings.Join(parts, ".*") + "$")
}

// compilePathGlob turns a file glob into an expression, where "*" stays
// inside one path segment and "**" crosses segments.
func compilePathGlob(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case glob[i] != '*':
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		case i+1 < len(glob) && glob[i+1] == '*':
			b.WriteString(".*")
			i++
		default:
			b.WriteString("[^/]*")
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
