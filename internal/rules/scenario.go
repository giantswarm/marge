package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/marge/internal/pr"
)

// ScenarioDir holds the fixtures, one directory per rule.
const ScenarioDir = "testdata"

// Scenario replays one recorded PR against the catalogue and states which
// rule must match it. A rule without both a scenario that matches and one
// that does not cannot land.
type Scenario struct {
	// Name says what the case shows, in the words of a test name.
	Name string `yaml:"name"`
	// Subject is the classified PR as the sweep saw it.
	Subject ScenarioSubject `yaml:"subject"`
	// Expect names the rule that must match, or "" for a case that must
	// match nothing.
	Expect ScenarioExpect `yaml:"expect"`

	// Path is where the fixture was read from.
	Path string `yaml:"-"`
	// Rule is the rule the fixture belongs to, from its directory name.
	Rule string `yaml:"-"`
}

// ScenarioSubject is the recorded PR. Logs are keyed "<source>:<check>" and
// hold the excerpt verbatim, as the runbook recorded it.
type ScenarioSubject struct {
	State     string                `yaml:"state"`
	Kind      string                `yaml:"kind"`
	Title     string                `yaml:"title"`
	Failing   []string              `yaml:"failing"`
	BaseState map[string]CheckState `yaml:"baseState"`
	// MissingContexts records the required contexts of the base branch that
	// the head never reported.
	MissingContexts []string          `yaml:"missingContexts"`
	Files           []string          `yaml:"files"`
	Logs            map[string]string `yaml:"logs"`
	// Pending records the checks that had not finished, with the message
	// each of them reported, verbatim as the gate wrote it.
	Pending []PendingCheck `yaml:"pending"`
}

// ScenarioExpect is the outcome the fixture asserts.
type ScenarioExpect struct {
	// Rule is the rule that must match, or "" for no match.
	Rule string `yaml:"rule"`
	// Check is the check the signal must have matched. Empty skips the
	// assertion.
	Check string `yaml:"check"`
	// Commands are the strings the output signal must have captured, in
	// order. Empty skips the assertion.
	Commands []string `yaml:"commands"`
}

// LoadScenarios reads every fixture under dir, which holds one directory per
// rule. A fixture that cannot be read is an error: a catalogue with an
// unreadable test is not a tested catalogue.
func LoadScenarios(dir string) ([]*Scenario, error) {
	ruleDirs, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading scenarios: %w", err)
	}

	var out []*Scenario
	for _, ruleDir := range ruleDirs {
		if !ruleDir.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, ruleDir.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading scenarios of %s: %w", ruleDir.Name(), err)
		}
		for _, file := range files {
			if file.IsDir() || !isRuleFile(file.Name()) {
				continue
			}
			path := filepath.Join(dir, ruleDir.Name(), file.Name())
			scenario, err := parseScenario(path, ruleDir.Name())
			if err != nil {
				return nil, err
			}
			out = append(out, scenario)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func parseScenario(path, rule string) (*Scenario, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)

	var scenario Scenario
	if err := dec.Decode(&scenario); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(new(Scenario)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: one scenario per file", path)
	}
	scenario.Path, scenario.Rule = path, rule
	if strings.TrimSpace(scenario.Name) == "" {
		return nil, fmt.Errorf("%s: name is required", path)
	}
	if _, known := remediableStates[scenario.Subject.State]; !known {
		return nil, fmt.Errorf("%s: unknown state %q", path, scenario.Subject.State)
	}
	return &scenario, nil
}

// Run replays the scenario against the catalogue and reports what went
// wrong, or "" when the outcome is the expected one.
func (s *Scenario) Run(catalogue *Catalogue) string {
	hit := catalogue.Match(s.subject())
	switch {
	case hit == nil && s.Expect.Rule == "":
		return ""
	case hit == nil:
		return fmt.Sprintf("expected rule %q, nothing matched", s.Expect.Rule)
	case s.Expect.Rule == "":
		return fmt.Sprintf("expected no match, rule %q matched on %q", hit.Rule.Name, hit.Check)
	case hit.Rule.Name != s.Expect.Rule:
		return fmt.Sprintf("expected rule %q, rule %q matched", s.Expect.Rule, hit.Rule.Name)
	case s.Expect.Check != "" && hit.Check != s.Expect.Check:
		return fmt.Sprintf("expected the signal on %q, it matched on %q", s.Expect.Check, hit.Check)
	case len(s.Expect.Commands) > 0 && !slices.Equal(hit.Commands, s.Expect.Commands):
		return fmt.Sprintf("expected the commands %q, it captured %q", s.Expect.Commands, hit.Commands)
	}
	return ""
}

func (s *Scenario) subject() *Subject {
	subject := &Subject{
		State:           remediableStates[s.Subject.State],
		Kind:            pr.Kind(s.Subject.Kind),
		Title:           s.Subject.Title,
		Failing:         s.Subject.Failing,
		BaseState:       s.Subject.BaseState,
		MissingContexts: s.Subject.MissingContexts,
		Pending:         s.Subject.Pending,
	}
	if len(s.Subject.Files) > 0 {
		subject.Files = func() []string { return s.Subject.Files }
	}
	if len(s.Subject.Logs) > 0 {
		subject.Log = func(source LogSource, check string, _ int) (string, bool) {
			excerpt, ok := s.Subject.Logs[string(source)+":"+check]
			return excerpt, ok
		}
	}
	return subject
}

// Coverage reports the rules that have no scenario matching them and the
// rules that have no scenario refusing them. A rule needs both: one that
// shows it fires on its real signal, and one that shows it does not fire on
// a failure it must leave alone.
type Coverage struct {
	NoPositive []string
	NoNegative []string
	// Orphans are scenario directories that name no rule of the catalogue.
	Orphans []string
}

// Covered reports whether every rule has both cases.
func (c Coverage) Covered() bool {
	return len(c.NoPositive) == 0 && len(c.NoNegative) == 0 && len(c.Orphans) == 0
}

// CheckCoverage matches the catalogue against its scenarios.
func CheckCoverage(catalogue *Catalogue, scenarios []*Scenario) Coverage {
	positive := make(map[string]bool)
	negative := make(map[string]bool)
	known := make(map[string]bool, len(catalogue.Rules))
	for _, rule := range catalogue.Rules {
		known[rule.Name] = true
	}

	var coverage Coverage
	seenDirs := make(map[string]bool)
	for _, scenario := range scenarios {
		if !known[scenario.Rule] && !seenDirs[scenario.Rule] {
			seenDirs[scenario.Rule] = true
			coverage.Orphans = append(coverage.Orphans, scenario.Rule)
		}
		if scenario.Expect.Rule == scenario.Rule {
			positive[scenario.Rule] = true
			continue
		}
		negative[scenario.Rule] = true
	}

	for _, rule := range catalogue.Rules {
		if !positive[rule.Name] {
			coverage.NoPositive = append(coverage.NoPositive, rule.Name)
		}
		if !negative[rule.Name] {
			coverage.NoNegative = append(coverage.NoNegative, rule.Name)
		}
	}
	slices.Sort(coverage.Orphans)
	return coverage
}
