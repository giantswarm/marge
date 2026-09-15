// Package policy reads the bot PR sweep policy files and resolves them into
// the policy one repository is swept under.
//
// Three files take part, applied in this order on top of the company
// defaults in pr.CompanyDefaults:
//
//	bot-prs-sweep/default.yaml        company defaults
//	bot-prs-sweep/team-<name>.yaml    one team's deviations, owned by the team
//	repositories/team-<name>.yaml     one repository's exception, on its entry
//
// Every file lives in giantswarm/github and is read from its default branch
// at the start of a sweep. A file that does not parse, or that names a key,
// a bot PR kind, an update type or a value the sweep does not know, is an
// error. A policy a team cannot read back is worse than no policy, so the
// sweep stops instead of quietly applying defaults.
package policy

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/marge/internal/pr"
)

// Document is one policy file. Every field is optional and an absent field
// keeps what the file before it said. UpdateTypes replaces the list of the
// kinds it names and leaves every other kind alone.
//
// The names a file writes are also held in their resolved form, which
// ParseDocument fills. Only ParseDocument builds a Document, so a Document
// that exists has been resolved, and applying one needs no second pass over
// the names and no error a caller has to discard.
type Document struct {
	UpdateTypes  map[string][]string  `yaml:"updateTypes"`
	Schedule     *string              `yaml:"schedule"`
	Rescue       *RescueDocument      `yaml:"rescue"`
	Concurrency  *ConcurrencyDocument `yaml:"concurrency"`
	ModelConfig  *string              `yaml:"modelConfig"`
	SlackChannel *string              `yaml:"slackChannel"`

	// updateTypes is UpdateTypes with every name resolved.
	updateTypes map[pr.Kind][]pr.UpdateType
	// resolved is set by ParseDocument alone. The exported fields exist
	// for the YAML decoder, so a caller can fill them by hand and reach a
	// document whose names were never checked; NewSet refuses that
	// document instead of applying a policy with an empty updateTypes.
	resolved bool
}

// RescueDocument is the rescue section of a policy file.
type RescueDocument struct {
	Enabled *bool           `yaml:"enabled"`
	Timeout *string         `yaml:"timeout"`
	Weekly  *int            `yaml:"weekly"`
	Budget  *BudgetDocument `yaml:"budget"`
	Confirm *string         `yaml:"confirm"`

	// timeout is Timeout resolved. It is zero when Timeout is absent.
	timeout time.Duration
}

// BudgetDocument is the rescue budget in US dollars. Neither figure is
// enforced yet; see pr.BudgetEnforced.
type BudgetDocument struct {
	PerRescue *float64 `yaml:"perRescue"`
	Weekly    *float64 `yaml:"weekly"`
}

// ConcurrencyDocument is the concurrency section of a policy file.
type ConcurrencyDocument struct {
	PerTeam *int `yaml:"perTeam"`
	PerRepo *int `yaml:"perRepo"`
}

// Exception is one repository's deviation, written under the botPRsSweep
// key of its entry in repositories/team-<name>.yaml. Three keys are allowed
// and each one only narrows: switch the sweep off, restrict the update
// types that merge, switch the rescues off.
type Exception struct {
	Enabled     *bool    `yaml:"enabled"`
	UpdateTypes []string `yaml:"updateTypes"`
	Rescue      *bool    `yaml:"rescue"`
}

// The ceilings on the concurrency a policy file may declare. GitHub answers
// a burst of writes with a secondary rate limit, which a sweep cannot tell
// apart from a repository it may not touch, so the ceiling is the sweep's
// and not the team's to raise.
const (
	maxPerTeam = 20
	maxPerRepo = 5
)

// The two values the schedule key takes. A team switches its scheduled
// sweep on in its own policy file, and off again without deleting the file.
const (
	scheduleEnabled  = "enabled"
	scheduleDisabled = "disabled"
)

// knownKinds maps the bot PR kind names a policy file may use to the kinds
// the sweep classifies PRs into.
var knownKinds = map[string]pr.Kind{
	"renovate":    pr.KindRenovate,
	"dependabot":  pr.KindDependabot,
	"align-files": pr.KindAlignFiles,
	"herald":      pr.KindHerald,
}

// knownUpdateTypes maps the update type names a policy file may use.
// "unknown" is deliberately absent: an update whose size the sweep could
// not read always waits for a person, and no file may declare it eligible.
var knownUpdateTypes = map[string]pr.UpdateType{
	"major":    pr.UpdateMajor,
	"minor":    pr.UpdateMinor,
	"patch":    pr.UpdatePatch,
	"digest":   pr.UpdateDigest,
	"pin":      pr.UpdatePin,
	"lockfile": pr.UpdateLockfile,
	"none":     pr.UpdateNone,
}

// ParseDocument reads one policy file. An unknown key, a value of the wrong
// type and an unknown kind, update type or enumerated value are all errors
// naming path.
func ParseDocument(path, content string) (*Document, error) {
	var doc Document
	if err := strictUnmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("parsing policy file %s: %w", path, err)
	}
	if err := doc.resolve(); err != nil {
		return nil, fmt.Errorf("policy file %s: %w", path, err)
	}
	doc.resolved = true
	return &doc, nil
}

// resolve checks every value the document names and holds the result, so
// apply reads a resolved document and never parses a name again.
func (d *Document) resolve() error {
	if len(d.UpdateTypes) > 0 {
		d.updateTypes = make(map[pr.Kind][]pr.UpdateType, len(d.UpdateTypes))
	}
	for kind, names := range d.UpdateTypes {
		known, ok := knownKinds[kind]
		if !ok {
			return fmt.Errorf("updateTypes: unknown bot PR kind %q: known kinds are %s", kind, sortedNames(knownKinds))
		}
		types, err := updateTypes(names)
		if err != nil {
			return fmt.Errorf("updateTypes.%s: %w", kind, err)
		}
		d.updateTypes[known] = types
	}
	if d.Schedule != nil && *d.Schedule != scheduleEnabled && *d.Schedule != scheduleDisabled {
		return fmt.Errorf("schedule: %q is neither %s nor %s", *d.Schedule, scheduleEnabled, scheduleDisabled)
	}
	if err := d.Rescue.resolve(); err != nil {
		return err
	}
	if c := d.Concurrency; c != nil {
		if c.PerTeam != nil && (*c.PerTeam < 1 || *c.PerTeam > maxPerTeam) {
			return fmt.Errorf("concurrency.perTeam: %d is outside 1 to %d", *c.PerTeam, maxPerTeam)
		}
		if c.PerRepo != nil && (*c.PerRepo < 1 || *c.PerRepo > maxPerRepo) {
			return fmt.Errorf("concurrency.perRepo: %d is outside 1 to %d", *c.PerRepo, maxPerRepo)
		}
	}
	return nil
}

func (r *RescueDocument) resolve() error {
	if r == nil {
		return nil
	}
	if r.Timeout != nil {
		timeout, err := time.ParseDuration(*r.Timeout)
		if err != nil {
			return fmt.Errorf("rescue.timeout: %q is not a duration such as 20m", *r.Timeout)
		}
		if timeout <= 0 {
			return fmt.Errorf("rescue.timeout: %s is not a positive duration", *r.Timeout)
		}
		r.timeout = timeout
	}
	if r.Weekly != nil && *r.Weekly < 0 {
		return fmt.Errorf("rescue.weekly: %d is negative", *r.Weekly)
	}
	if b := r.Budget; b != nil {
		if b.PerRescue != nil && *b.PerRescue < 0 {
			return fmt.Errorf("rescue.budget.perRescue: %v is negative", *b.PerRescue)
		}
		if b.Weekly != nil && *b.Weekly < 0 {
			return fmt.Errorf("rescue.budget.weekly: %v is negative", *b.Weekly)
		}
	}
	if r.Confirm != nil {
		switch pr.Confirm(*r.Confirm) {
		case pr.ConfirmPerPR, pr.ConfirmPerSweep:
		default:
			return fmt.Errorf("rescue.confirm: %q is neither %s nor %s", *r.Confirm, pr.ConfirmPerPR, pr.ConfirmPerSweep)
		}
	}
	return nil
}

// resolve checks the exception's update type names and returns them
// resolved. The list is nil when the exception names none, which is what
// tells apply to leave the team's lists alone.
func (e Exception) resolve(repo string) ([]pr.UpdateType, error) {
	if e.UpdateTypes == nil {
		return nil, nil
	}
	types, err := updateTypes(e.UpdateTypes)
	if err != nil {
		return nil, fmt.Errorf("botPRsSweep.updateTypes of repository %s: %w", repo, err)
	}
	// An exception reaches the kinds that name a version. UpdateNone belongs
	// to Align files and Herald alone, so a list that holds it intersects
	// every versioned list to nothing while reading as a restriction.
	if slices.Contains(types, pr.UpdateNone) {
		return nil, fmt.Errorf("botPRsSweep.updateTypes of repository %s: %q reaches no kind an exception covers: an exception restricts the Renovate and Dependabot lists, and those always name a version", repo, pr.UpdateNone)
	}
	return types, nil
}

// updateTypes maps the names of an update type list to the types.
func updateTypes(names []string) ([]pr.UpdateType, error) {
	out := make([]pr.UpdateType, 0, len(names))
	for _, name := range names {
		known, ok := knownUpdateTypes[name]
		if !ok {
			return nil, fmt.Errorf("unknown update type %q: known types are %s", name, sortedNames(knownUpdateTypes))
		}
		out = append(out, known)
	}
	return out, nil
}

// strictUnmarshal decodes content into out and refuses every key out does
// not declare, so a misspelled policy key is an error and not a silent
// default. An empty document leaves out untouched.
func strictUnmarshal(content string, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(content))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// strictDecodeNode decodes one YAML node strictly. The node is written back
// out first because the decoder carries the strict setting, not the node.
func strictDecodeNode(node *yaml.Node, out any) error {
	buf, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	return strictUnmarshal(string(buf), out)
}

func sortedNames[V any](m map[string]V) string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
