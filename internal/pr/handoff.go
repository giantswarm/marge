package pr

import (
	"path"
	"slices"
	"time"
)

// RescueCandidate is a PR that passed every rescue precondition the sweep
// can decide from the PR alone. The run-level step decides the rest: the
// team's weekly count, the PR's state just before handoff, an open
// teammate PR on the same module and the run's toolchain.
type RescueCandidate struct {
	// HeadSHA is the head the sweep classified. A head that moved before
	// the handoff is a different change, which the next sweep classifies.
	HeadSHA string
	// Checks are the failing checks, and CheckURLs where each one's build
	// or job lives.
	Checks    []string
	CheckURLs map[string]string
	// Signature and Excerpt describe the failure no rule recognised.
	Signature string
	Excerpt   string
	// Files are the paths of the PR diff. Nil when the comparison could not
	// be had or was truncated, in which case no module is known.
	Files []string
}

// Modules returns the directories of the dependency manifests the diff
// changes: the modules a teammate's PR would collide with.
func (c *RescueCandidate) Modules() []string {
	var dirs []string
	for _, file := range c.Files {
		if !manifests[path.Base(file)] {
			continue
		}
		dir := path.Dir(file)
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// manifests are the files that declare a module's dependencies. A bot PR
// changes at least one of them; a teammate's PR that changes a file under
// the same directory works on the same module.
var manifests = map[string]bool{
	"go.mod":            true,
	"go.sum":            true,
	"package.json":      true,
	"package-lock.json": true,
	"Chart.yaml":        true,
	"Chart.lock":        true,
	"requirements.txt":  true,
	"pyproject.toml":    true,
}

// WriteTarget says where a rescue of a PR of this kind pushes its fix.
type WriteTarget string

const (
	// WritePRBranch: the fix is a commit on the PR's own branch.
	WritePRBranch WriteTarget = "pr-branch"
	// WriteDefaultBranch: the PR's branch is regenerated on every cycle, so
	// a fix on it does not survive. The fix is a PR against the
	// repository's default branch, or against the generator when the
	// generated content itself is wrong.
	WriteDefaultBranch WriteTarget = "default-branch"
)

// RescueWriteTarget returns where a rescue of a PR of kind pushes.
func RescueWriteTarget(kind Kind) WriteTarget {
	if kind == KindAlignFiles {
		return WriteDefaultBranch
	}
	return WritePRBranch
}

// Handoff is what a sweep with the rescue action hands to the agent it runs
// inside: the PRs to rescue, grouped by repository, and what is left of the
// team's weekly count.
type Handoff struct {
	Team string
	// TriggeredBy names who started the run: the schedule, or the person a
	// rescue was asked for by. The agent writes it on every marker.
	TriggeredBy string
	// Toolchain is the language the run's image builds. Empty hands over
	// PRs of every language.
	Toolchain string
	Budget    HandoffBudget
	Repos     []HandoffRepo
	// Refused are the candidates the run-level checks turned away.
	Refused []HandoffRefusal
	// Error says why nothing was handed over, for instance a weekly count
	// that could not be read. A count that cannot be read hands nothing
	// over: an unknown count is not a count under the cap.
	Error string
}

// HandoffBudget is the team's rescue bounds and how much of the week's
// count is spent.
type HandoffBudget struct {
	Weekly int
	// Counted says the week's rescues were counted. A run with no candidate
	// counts nothing, and Used and Remaining are then unknown, not zero.
	Counted   bool
	Used      int
	Remaining int
	// Timeout is how long one run may take, and PerRescueUSD what the team
	// declared one rescue may cost. Neither is enforced by marge.
	Timeout      time.Duration
	PerRescueUSD float64
}

// HandoffRepo is one repository of the handoff. The agent works on one
// repository at a time.
type HandoffRepo struct {
	Repo     string
	Language string
	// LanguageNote says where the language came from when the repository's
	// content contradicts the team file.
	LanguageNote string
	PRs          []HandoffPR
}

// HandoffPR is one PR to rescue.
type HandoffPR struct {
	PR          PRInfo
	Kind        Kind
	UpdateType  UpdateType
	HeadSHA     string
	WriteTarget WriteTarget
	Checks      []string
	CheckURLs   map[string]string
	Signature   string
	Excerpt     string
}

// HandoffRefusal is a candidate the run did not hand over, and why.
type HandoffRefusal struct {
	PR     PRInfo
	Reason string
}

// Count returns how many PRs the handoff carries.
func (h *Handoff) Count() int {
	if h == nil {
		return 0
	}
	n := 0
	for _, repo := range h.Repos {
		n += len(repo.PRs)
	}
	return n
}
