package pr

import (
	"slices"
	"time"
)

// Confirm says who confirms a rescue before it is dispatched: the person
// confirms every PR, or once for the whole sweep.
type Confirm string

const (
	ConfirmPerPR    Confirm = "per-pr"
	ConfirmPerSweep Confirm = "per-sweep"
)

// Budget is what a team is willing to spend on rescues, in US dollars.
// Both figures are part of the team contract and neither is enforced: the
// platform bounds no turn by a budget, so the cost a rescue reports is
// written on its marker and summed, never stopped at. BudgetEnforced says so
// in every outcome the sweep records.
type Budget struct {
	PerRescueUSD float64
	WeeklyUSD    float64
}

// RescuesDispatched reports whether this build hands rescues over. It does:
// the rescue action hands the PRs no rule recognised to the agent the run
// executes inside, and the weekly count bounds how many.
const RescuesDispatched = true

// TimeoutEnforced reports whether this build bounds a rescue run by the
// policy's timeout. It is false until the trigger that starts the run
// cancels it at the deadline.
const TimeoutEnforced = false

// BudgetEnforced reports whether this build enforces the rescue budget. It
// is false while the platform bounds no turn by a budget.
const BudgetEnforced = false

// RescuePolicy bounds the rescues of one team. Timeout is the wall-clock
// budget of a rescue run; Weekly is how many rescues the team spends per
// week, counted from the markers of the last seven days. See
// TimeoutEnforced and BudgetEnforced for what this build applies.
type RescuePolicy struct {
	Enabled bool
	Timeout time.Duration
	Weekly  int
	Budget  Budget
	Confirm Confirm
}

// Concurrency bounds how much of a sweep runs at once: PerTeam is the
// number of repositories swept in parallel, PerRepo the number of PRs of
// one repository. Two PRs of one repository race for the same base branch,
// so PerRepo defaults to 1.
type Concurrency struct {
	PerTeam int
	PerRepo int
}

// ReadConcurrency bounds a run that classifies and does nothing else. The
// policy's own bounds exist for the writes: PerRepo is 1 so that two merges
// never race for one base branch, and the product is held small so a sweep
// cannot empty the write budget in a minute. A classify-only run makes
// neither a merge nor a comment, so it is bounded by how many reads GitHub
// answers at once; the label writes it makes are held to the policy's
// product on their own.
var ReadConcurrency = Concurrency{PerTeam: 15, PerRepo: 4}

// ChangelogPolicy is the changelog entry a team writes for a bot PR: which
// file, which heading and section of it, and the line itself. The sweep writes
// it on a PR it is about to approve, and a person asks for it on the PRs they
// pick.
type ChangelogPolicy struct {
	// Enabled lets the sweep write the entry on its own, on a PR whose
	// checks are green and before it approves anything. A team that does
	// not want its bot PRs to carry an entry switches it off in its own
	// file. The changelog tool writes on demand whatever this says.
	Enabled bool
	// Path is the file the entry is added to, relative to the repository
	// root.
	Path string
	// Heading is the heading the entry goes under, and Section the
	// subheading inside it. The section is created under the heading when
	// the file does not carry it yet.
	Heading string
	Section string
	// Template is the line, as a Go text/template over ChangelogFacts.
	Template string
}

// ChangelogFacts are the values a changelog template may name.
type ChangelogFacts struct {
	// Dependency is what the update moves, From and To the versions it
	// moves between. From is empty when the title names only the target.
	Dependency string
	From       string
	To         string
	// PR is the reference of the pull request, owner/repo#number.
	PR string
	// Repository is owner/repo, Kind the bot that authored the PR, and
	// UpdateType the size of the update.
	Repository string
	Kind       string
	UpdateType string
}

// ChangelogDefaults is the changelog entry of a team that writes none: a
// Keep a Changelog file, the entry under the unreleased heading's Changed
// section, and one line naming the dependency and both versions. The entry is
// written where a repository cuts its release from that file; a repository
// that generates its release notes from its commits publishes the update
// already and earns none. A team that wants no entry at all says so in its own
// file.
func ChangelogDefaults() ChangelogPolicy {
	return ChangelogPolicy{
		Enabled:  true,
		Path:     "CHANGELOG.md",
		Heading:  "## [Unreleased]",
		Section:  "### Changed",
		Template: "- Update {{ .Dependency }}{{ if .From }} from {{ .From }}{{ end }} to {{ .To }} ({{ .PR }})",
	}
}

// Policy is the resolved bot PR sweep policy of one repository: the company
// defaults, the owning team's deviations and the repository's own exception,
// merged in that order.
type Policy struct {
	// Sweep is false when the repository's exception switches the sweep
	// off. Every PR of that repository is then skipped.
	Sweep bool
	// UpdateTypes lists the update types that merge when green, per bot PR
	// kind. A kind that is absent merges nothing.
	UpdateTypes map[Kind][]UpdateType
	Rescue      RescuePolicy
	Concurrency Concurrency
	Changelog   ChangelogPolicy
	ModelConfig string
	// Summary says the team's sweep summary is posted, to the notices
	// channel of its channel file. The company defaults post none.
	Summary bool
	// Sources names every file that produced this policy, in the order the
	// files were applied, so one outcome explains its own decision.
	Sources []string
}

// CompanyDefaults is the policy that applies where no file says otherwise:
// patch and minor updates merge when green for Renovate and Dependabot,
// Align files and Herald PRs carry no version change and always merge, a
// major and an update whose size could not be read wait for a person. The
// rescues are off.
func CompanyDefaults() Policy {
	return Policy{
		Sweep: true,
		UpdateTypes: map[Kind][]UpdateType{
			KindRenovate:   {UpdatePatch, UpdateMinor, UpdateDigest, UpdatePin, UpdateLockfile},
			KindDependabot: {UpdatePatch, UpdateMinor, UpdateDigest, UpdatePin, UpdateLockfile},
			KindAlignFiles: {UpdateNone},
			KindHerald:     {UpdateNone},
			// A sync PR merges only where its team opts in: the sync lands a
			// whole upstream release in a chart the team owns.
			KindUpstreamSync: {},
		},
		Rescue: RescuePolicy{
			Enabled: false,
			Timeout: 20 * time.Minute,
			Weekly:  5,
			Confirm: ConfirmPerPR,
		},
		Concurrency: Concurrency{PerTeam: 5, PerRepo: 1},
		Changelog:   ChangelogDefaults(),
		ModelConfig: "default-model-config",
		Sources:     []string{"built-in company defaults"},
	}
}

// Eligible reports whether a green PR of this kind and update type merges
// under the policy.
func (p Policy) Eligible(kind Kind, updateType UpdateType) bool {
	return slices.Contains(p.UpdateTypes[kind], updateType)
}

// AllowsUpdate reports whether any bot PR kind of the policy merges this
// update type.
func (p Policy) AllowsUpdate(updateType UpdateType) bool {
	for _, types := range p.UpdateTypes {
		if slices.Contains(types, updateType) {
			return true
		}
	}
	return false
}

// DeclaredUnenforced names the caps the policy declares that this build
// does not enforce, so a team is told rather than left to assume. It is
// empty while the team has the rescues off: a cap on a rescue the team does
// not want bounds nothing either way.
func (p Policy) DeclaredUnenforced() []string {
	if !p.Rescue.Enabled {
		return nil
	}
	var out []string
	if !TimeoutEnforced && p.Rescue.Timeout > 0 {
		out = append(out, "rescue.timeout")
	}
	if !BudgetEnforced {
		if p.Rescue.Budget.PerRescueUSD > 0 {
			out = append(out, "rescue.budget.perRescue")
		}
		if p.Rescue.Budget.WeeklyUSD > 0 {
			out = append(out, "rescue.budget.weekly")
		}
	}
	return out
}

// Clone returns a copy that shares nothing with the original, so one
// repository's exception cannot reach another repository's policy.
func (p Policy) Clone() Policy {
	out := p
	out.UpdateTypes = make(map[Kind][]UpdateType, len(p.UpdateTypes))
	for kind, types := range p.UpdateTypes {
		out.UpdateTypes[kind] = slices.Clone(types)
	}
	out.Sources = slices.Clone(p.Sources)
	return out
}
