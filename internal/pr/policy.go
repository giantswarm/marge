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
// Both figures are part of the team contract and neither is enforced yet:
// a per-rescue budget needs execution_budget_usd on the run, and a weekly
// budget needs the cost of finished runs to be readable. BudgetEnforced
// says so in every outcome the sweep records.
type Budget struct {
	PerRescueUSD float64
	WeeklyUSD    float64
}

// BudgetEnforced reports whether this build enforces the rescue budget.
// It is false until the platform accepts a budget on a run and reports the
// cost of a finished one.
const BudgetEnforced = false

// RescuePolicy bounds the rescues of one team. Timeout is the wall-clock
// budget of a single rescue and becomes the run's execution timeout; Weekly
// is how many rescues the team spends per week.
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
	// Schedule reports whether the scheduled sweep runs for the team.
	Schedule     bool
	Rescue       RescuePolicy
	Concurrency  Concurrency
	ModelConfig  string
	SlackChannel string
	// Sources names every file that produced this policy, in the order the
	// files were applied, so one outcome explains its own decision.
	Sources []string
}

// CompanyDefaults is the policy that applies where no file says otherwise:
// patch and minor updates merge when green for Renovate and Dependabot,
// Align files and Herald PRs carry no version change and always merge, a
// major and an update whose size could not be read wait for a person. The
// schedule and the rescues are off.
func CompanyDefaults() Policy {
	return Policy{
		Sweep: true,
		UpdateTypes: map[Kind][]UpdateType{
			KindRenovate:   {UpdatePatch, UpdateMinor, UpdateDigest, UpdatePin, UpdateLockfile},
			KindDependabot: {UpdatePatch, UpdateMinor, UpdateDigest, UpdatePin, UpdateLockfile},
			KindAlignFiles: {UpdateNone},
			KindHerald:     {UpdateNone},
		},
		Schedule: false,
		Rescue: RescuePolicy{
			Enabled: false,
			Timeout: 20 * time.Minute,
			Weekly:  5,
			Confirm: ConfirmPerPR,
		},
		Concurrency: Concurrency{PerTeam: 5, PerRepo: 1},
		ModelConfig: "default-model-config",
		Sources:     []string{"built-in company defaults"},
	}
}

// Eligible reports whether a green PR of this kind and update type merges
// under the policy.
func (p Policy) Eligible(kind Kind, updateType UpdateType) bool {
	return slices.Contains(p.UpdateTypes[kind], updateType)
}

// DeclaredUnenforced names the caps the policy declares that this build
// does not enforce, so a team is told rather than left to assume. It is
// empty while the rescues are off: an unenforced cap on a rescue that never
// runs bounds nothing.
func (p Policy) DeclaredUnenforced() []string {
	if !p.Rescue.Enabled || BudgetEnforced {
		return nil
	}
	var out []string
	if p.Rescue.Budget.PerRescueUSD > 0 {
		out = append(out, "rescue.budget.perRescue")
	}
	if p.Rescue.Budget.WeeklyUSD > 0 {
		out = append(out, "rescue.budget.weekly")
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
