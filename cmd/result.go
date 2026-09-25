package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/giantswarm/marge/internal/pr"
)

// SweepResult is the structured JSON output returned by the sweep MCP tool.
type SweepResult struct {
	Summary SweepSummary `json:"summary"`
	// Rules says which rule catalogue the sweep ran, and what it could not
	// use. An absent catalogue leaves every remedy refused.
	Rules *SweepRules `json:"rules,omitempty"`
	// Unhandled groups the failures no rule recognised, most frequent
	// first. A signature here is what `marge rules draft` takes.
	Unhandled []SweepUnhandled `json:"unhandled,omitempty"`
	Merged    []SweepPREntry   `json:"merged,omitempty"`
	// AutoMerge lists the PRs handed to GitHub's auto-merge, which merges
	// them when their last requirement is met.
	AutoMerge []SweepPREntry `json:"auto_merge,omitempty"`
	// Remedied lists the PRs a rule of the catalogue acted on in this run:
	// a rerun, a retry, a branch update, a wait marker or a close.
	Remedied         []SweepPREntry `json:"remedied,omitempty"`
	SecurityFailures []SweepPREntry `json:"security_failures,omitempty"`
	ActionRequired   []SweepPREntry `json:"action_required,omitempty"`
	// Eligible lists green PRs the run did not merge because the merge step
	// was not among its actions. Every green PR of a list is one.
	Eligible []SweepPREntry `json:"eligible,omitempty"`
	// Unclassified lists PRs that carry no marge/<class> label, so no sweep
	// has decided them yet. Only a read of the stored classification
	// produces them: a run that classifies decides every PR it reads.
	Unclassified []SweepPREntry `json:"unclassified,omitempty"`
	// Stale lists failing PRs whose head is behind the base branch and whose
	// every failing check is green on the base branch head: the failure was
	// fixed on the base branch after the PR's last build. The remedy is a
	// branch refresh (the refresh action), not a rescue, so they are excluded from
	// action_required.
	Stale []SweepPREntry `json:"stale,omitempty"`
	// Refreshed lists stale PRs whose branch was updated from its base in
	// this run. CI is running again; the next sweep decides what they are.
	Refreshed []SweepPREntry `json:"refreshed,omitempty"`
	// Cancelled lists failing PRs whose every failing check is a CircleCI
	// build that CircleCI itself auto-cancelled: there is no verdict on the
	// code yet. The remedy is a retry (the retry action), not a rescue, so
	// they are excluded from action_required. A build cancelled behind a
	// newer head is listed too but never retried: the new head's own build
	// is the verdict.
	Cancelled []SweepPREntry `json:"cancelled,omitempty"`
	// Retried lists cancelled PRs whose workflow was rerun on the same
	// commit in this run. CI is running again; the next sweep decides.
	Retried []SweepPREntry `json:"retried,omitempty"`
	// CIUnavailable lists PRs whose CI could not run because a GitHub Actions
	// budget / spending-limit block prevented every job from starting. These
	// are NOT failures: the remedy is to raise or await the Actions budget,
	// so they are reported separately and excluded from action_required.
	CIUnavailable []SweepPREntry `json:"ci_unavailable,omitempty"`
	// CINoVerdict lists PRs whose every failing check established nothing
	// about the code: a cancelled job, or a pipeline a CircleCI project
	// setting refuses. These are NOT failures, and a security check in this
	// shape is NOT a finding, so they are excluded from action_required and
	// security_failures. Each entry's detail names its own remedy.
	CINoVerdict []SweepPREntry `json:"ci_no_verdict,omitempty"`
	// Obsolete lists bot PRs that are not worth fixing: a sibling PR carries
	// a higher version of the same dependency, or the diff changes nothing
	// that executes. Each entry's reason says which. The remedy is to close
	// them, so they are excluded from action_required.
	Obsolete []SweepPREntry `json:"obsolete,omitempty"`
	// Waiting lists PRs whose required checks have not all reported: a
	// required context is pending or was never reported. The sweep never
	// merges past a required check; the next sweep decides.
	Waiting []SweepPREntry `json:"waiting,omitempty"`
	Skipped []SweepPREntry `json:"skipped,omitempty"`
	// RepositoriesFailed lists the repositories whose PRs could not be
	// listed, so a partial sweep is visible as such.
	RepositoriesFailed []SweepRepoFailure `json:"repositories_failed,omitempty"`
	// Findings groups the repository settings that hold PRs back, the
	// setting on the most PRs first. The sweep acts on none of them: the
	// fix belongs to the repository.
	Findings []SweepFinding `json:"findings,omitempty"`
}

// SweepFinding is one repository setting that holds PRs back, with the
// repositories it holds them in.
type SweepFinding struct {
	Cause string `json:"cause"`
	// PRs counts the pull requests the setting holds across every
	// repository of this finding.
	PRs int `json:"prs"`
	// Repositories are the repositories the setting holds PRs in, the one
	// with the most PRs first.
	Repositories []SweepFindingRepo `json:"repositories"`
}

// SweepFindingRepo is one repository of a finding.
type SweepFindingRepo struct {
	Repo string `json:"repo"`
	PRs  int    `json:"prs"`
	// Detail carries what the cause needs to be acted on, for instance the
	// required context that never reported.
	Detail string `json:"detail,omitempty"`
}

// SweepRepoFailure names a repository the sweep could not list.
type SweepRepoFailure struct {
	Repo  string `json:"repo"`
	Error string `json:"error"`
}

// SweepSummary contains aggregate counts from the sweep.
//
// Failed and SecurityFailures are disjoint: Failed counts only the
// non-security failure entries, so consumers can use
// Failed + SecurityFailures to get the total number of action-required
// PRs without double-counting.
// SweepUnhandled is one shape of failure the catalogue does not recognise,
// with the PRs that carry it.
type SweepUnhandled struct {
	Signature string   `json:"signature"`
	Checks    []string `json:"checks"`
	Count     int      `json:"count"`
	PRs       []string `json:"prs"`
	// Excerpt is the log a rule would match against, from the first PR of
	// the group. Empty when no rule asked for a log.
	Excerpt string `json:"excerpt,omitempty"`
}

// SweepRules reports the rule catalogue of one sweep.
type SweepRules struct {
	// Source names the repository, ref and directory, or the local path.
	Source string `json:"source"`
	// Ref is the branch the catalogue was read from, absent for a local
	// directory.
	Ref string `json:"ref,omitempty"`
	// Digest identifies the exact documents this sweep ran, whether they
	// came from a repository or a local directory.
	Digest string `json:"digest,omitempty"`
	// Loaded counts the rules the sweep could use.
	Loaded int `json:"loaded"`
	// Skipped names the documents that failed to validate, with the reason.
	// They cost their own rule and nothing more.
	Skipped []SweepSkippedRule `json:"skipped,omitempty"`
	// Error says why no catalogue could be read at all.
	Error string `json:"error,omitempty"`
}

// SweepSkippedRule is one document the catalogue could not use.
type SweepSkippedRule struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type SweepSummary struct {
	Total  int `json:"total"`
	Merged int `json:"merged"`
	// AutoMerge counts the PRs left to GitHub's own auto-merge. They are not
	// merged: GitHub fires it when the last requirement is met.
	AutoMerge int `json:"auto_merge"`
	// Remedied counts the PRs a catalogue rule acted on.
	Remedied         int `json:"remedied"`
	Failed           int `json:"failed"`
	SecurityFailures int `json:"security_failures"`
	// CIUnavailable counts PRs whose CI could not run because of a GitHub
	// Actions budget block. It is disjoint from Failed and SecurityFailures.
	CIUnavailable int `json:"ci_unavailable"`
	// CINoVerdict counts PRs whose every failing check established nothing
	// about the code. It is disjoint from Failed and SecurityFailures.
	CINoVerdict int `json:"ci_no_verdict"`
	// Stale counts failing PRs whose failure is already fixed on the base
	// branch (see SweepResult.Stale); Refreshed counts the stale PRs whose
	// branch was updated in this run. Both are disjoint from Failed.
	Stale     int `json:"stale"`
	Refreshed int `json:"refreshed"`
	// Cancelled counts failing PRs whose failing builds CircleCI itself
	// cancelled (see SweepResult.Cancelled); Retried counts the cancelled
	// PRs whose builds were retried in this run. Both are disjoint from
	// Failed.
	Cancelled int `json:"cancelled"`
	Retried   int `json:"retried"`
	// Obsolete counts bot PRs that are not worth fixing, whether a
	// higher-version sibling replaced them or their diff changes nothing
	// that executes. Disjoint from Failed.
	Obsolete int `json:"obsolete"`
	// Waiting counts PRs whose required checks have not all reported.
	Waiting int `json:"waiting"`
	Skipped int `json:"skipped"`
	// Eligible counts green PRs left unmerged because the merge step was
	// not among the run's actions.
	Eligible int `json:"eligible"`
	// Unclassified counts PRs no sweep has labelled. Only a read of the
	// stored classification produces them.
	Unclassified int `json:"unclassified"`
}

// SweepPREntry represents a single PR in the sweep results.
type SweepPREntry struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Kind is the bot that authored the PR; UpdateType the size of the
	// update it carries; Label the marge/<class> label that is on
	// the PR after the sweep, empty when nothing was written.
	Kind       string `json:"kind,omitempty"`
	UpdateType string `json:"update_type,omitempty"`
	Label      string `json:"label,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	AgeDays    int    `json:"age_days,omitempty"`
	// Dependency, VersionFrom and VersionTo are the title read as an
	// update: what moves, and between which versions. A title that names
	// no version leaves both versions empty, and a title in no known shape
	// leaves all three empty.
	Dependency  string `json:"dependency,omitempty"`
	VersionFrom string `json:"version_from,omitempty"`
	VersionTo   string `json:"version_to,omitempty"`
	// Rescue describes the most recent prior automated rescue attempt
	// found on the PR (an ai-rescue marker comment), if any. Consumers
	// dispatching rescue agents should skip entries with a non-stale
	// failed rescue and escalate them to a human instead.
	Rescue *SweepRescueInfo `json:"rescue,omitempty"`
	// Reason says why an obsolete PR is obsolete: "superseded" or "no_op".
	// Only set on entries in the obsolete list.
	Reason string `json:"reason,omitempty"`
	// Policy is the sweep policy this PR was decided under, so an outcome
	// explains itself without the reader resolving the files again.
	Policy *SweepPolicyInfo `json:"policy,omitempty"`
}

// SweepPolicyInfo is the JSON projection of a pr.Policy.
type SweepPolicyInfo struct {
	Sweep bool `json:"sweep"`
	// UpdateTypes lists the update types that merge when green, per bot PR
	// kind.
	UpdateTypes map[string][]string `json:"update_types"`
	Rescue      SweepRescuePolicy   `json:"rescue"`
	Concurrency SweepConcurrency    `json:"concurrency"`
	ModelConfig string              `json:"model_config,omitempty"`
	// Summary says the team's summary is posted to the notices channel of
	// its channel file.
	Summary bool `json:"summary"`
	// Sources names the files that produced the policy, in the order they
	// were applied.
	Sources []string `json:"sources,omitempty"`
}

// SweepRescuePolicy is the rescue section of a resolved policy.
type SweepRescuePolicy struct {
	Enabled bool   `json:"enabled"`
	Timeout string `json:"timeout,omitempty"`
	Weekly  int    `json:"weekly"`
	// BudgetPerRescueUSD and BudgetWeeklyUSD are what the team declared.
	// BudgetEnforced says whether this build applies them; it is false
	// until the platform accepts a budget on a run and reports the cost of
	// a finished one.
	BudgetPerRescueUSD float64 `json:"budget_per_rescue_usd,omitempty"`
	BudgetWeeklyUSD    float64 `json:"budget_weekly_usd,omitempty"`
	BudgetEnforced     bool    `json:"budget_enforced"`
	// RescuesDispatched says whether this build dispatches a rescue at all.
	// While it is false Timeout and Weekly are declared and neither of them
	// bounds anything.
	RescuesDispatched bool   `json:"rescues_dispatched"`
	Confirm           string `json:"confirm,omitempty"`
}

// SweepConcurrency is the concurrency section of a resolved policy.
type SweepConcurrency struct {
	PerTeam int `json:"per_team"`
	PerRepo int `json:"per_repo"`
}

// policyInfo projects a resolved policy into its JSON shape.
func policyInfo(resolved *pr.Policy) *SweepPolicyInfo {
	if resolved == nil {
		return nil
	}
	types := make(map[string][]string, len(resolved.UpdateTypes))
	for kind, updateTypes := range resolved.UpdateTypes {
		names := make([]string, 0, len(updateTypes))
		for _, updateType := range updateTypes {
			names = append(names, string(updateType))
		}
		sort.Strings(names)
		types[string(kind)] = names
	}
	info := &SweepPolicyInfo{
		Sweep:       resolved.Sweep,
		UpdateTypes: types,
		Rescue: SweepRescuePolicy{
			Enabled:            resolved.Rescue.Enabled,
			Weekly:             resolved.Rescue.Weekly,
			BudgetPerRescueUSD: resolved.Rescue.Budget.PerRescueUSD,
			BudgetWeeklyUSD:    resolved.Rescue.Budget.WeeklyUSD,
			BudgetEnforced:     pr.BudgetEnforced,
			RescuesDispatched:  pr.RescuesDispatched,
			Confirm:            string(resolved.Rescue.Confirm),
		},
		Concurrency: SweepConcurrency{PerTeam: resolved.Concurrency.PerTeam, PerRepo: resolved.Concurrency.PerRepo},
		ModelConfig: resolved.ModelConfig,
		Summary:     resolved.Summary,
		Sources:     resolved.Sources,
	}
	if resolved.Rescue.Timeout > 0 {
		info.Rescue.Timeout = resolved.Rescue.Timeout.String()
	}
	return info
}

// SweepRescueInfo is the JSON projection of a pr.RescueMarker.
type SweepRescueInfo struct {
	Tool    string `json:"tool,omitempty"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	At      string `json:"at,omitempty"`
	// Stale is true when the PR content changed since the rescue attempt
	// -- the attempt no longer describes the current code and the PR is
	// fair game for another rescue.
	Stale bool `json:"stale"`
	// Rebased is true when the PR head moved since the rescue attempt but
	// the change did not (a Renovate rebase onto a newer base): the marker
	// still describes the current code and Stale is false.
	Rebased bool `json:"rebased"`
}

func buildSweepResult(status *pr.PRStatus, failed []repoFailure, sweepRules *SweepRules) SweepResult {
	counts := status.Summary()
	total := status.Len()
	securityEntries := status.SecurityFailedEntries()
	blockedEntries := status.BlockedEntries()

	result := SweepResult{
		Rules: sweepRules,
		Summary: SweepSummary{
			Total:            total,
			Merged:           counts.Merged,
			AutoMerge:        counts.AutoMerge,
			Remedied:         counts.Remedied,
			Failed:           counts.Failed - len(securityEntries),
			SecurityFailures: len(securityEntries),
			CIUnavailable:    counts.Blocked,
			CINoVerdict:      counts.NoVerdict,
			Stale:            counts.Stale,
			Refreshed:        counts.Refreshed,
			Cancelled:        counts.Cancelled,
			Retried:          counts.Retried,
			Obsolete:         counts.Obsolete,
			Waiting:          counts.Waiting,
			Skipped:          counts.Skipped,
			Eligible:         counts.Eligible,
			Unclassified:     counts.Unclassified,
		},
	}
	for _, f := range failed {
		result.RepositoriesFailed = append(result.RepositoriesFailed, SweepRepoFailure{Repo: f.Repo, Error: f.Err})
	}
	result.Unhandled = groupUnhandled(status.UnhandledEntries())
	result.Findings = groupFindings(status.FindingEntries())

	now := time.Now()
	toEntry := func(e pr.StatusEntry) SweepPREntry {
		entry := SweepPREntry{
			Owner:  e.PR.Owner,
			Repo:   e.PR.Repo,
			Number: e.PR.Number,
			Title:  e.PR.Title,
			URL:    e.PR.URL,
			Status: e.State.String(),
			Detail: e.Detail,
			Reason: string(e.ObsoleteReason),
			Kind:   string(e.Kind),
		}
		entry.Dependency = pr.ExtractUpdatedDependency(e.PR.Title)
		entry.VersionFrom, entry.VersionTo = pr.ExtractVersions(e.PR.Title)
		if e.UpdateType != "" {
			entry.UpdateType = string(e.UpdateType)
		}
		entry.Label = e.Label
		entry.Policy = policyInfo(e.Policy)
		if !e.PR.CreatedAt.IsZero() {
			entry.CreatedAt = e.PR.CreatedAt.UTC().Format(time.RFC3339)
			entry.AgeDays = pr.AgeDays(e.PR.CreatedAt, now)
		}
		if e.Rescue != nil {
			entry.Rescue = &SweepRescueInfo{
				Tool:    e.Rescue.Tool,
				Outcome: e.Rescue.Outcome,
				Reason:  e.Rescue.Reason,
				Stale:   e.Rescue.Stale,
				Rebased: e.Rescue.Rebased,
			}
			if !e.Rescue.At.IsZero() {
				entry.Rescue.At = e.Rescue.At.UTC().Format(time.RFC3339)
			}
		}
		return entry
	}

	for _, e := range status.MergedEntries() {
		result.Merged = append(result.Merged, toEntry(e))
	}

	for _, e := range status.AutoMergeEntries() {
		result.AutoMerge = append(result.AutoMerge, toEntry(e))
	}

	for _, e := range status.RemediedEntries() {
		result.Remedied = append(result.Remedied, toEntry(e))
	}

	for _, e := range securityEntries {
		result.SecurityFailures = append(result.SecurityFailures, toEntry(e))
	}

	for _, e := range blockedEntries {
		result.CIUnavailable = append(result.CIUnavailable, toEntry(e))
	}

	for _, e := range status.NoVerdictEntries() {
		result.CINoVerdict = append(result.CINoVerdict, toEntry(e))
	}

	for _, e := range status.ObsoleteEntries() {
		result.Obsolete = append(result.Obsolete, toEntry(e))
	}

	for _, e := range status.StaleEntries() {
		result.Stale = append(result.Stale, toEntry(e))
	}

	for _, e := range status.RefreshedEntries() {
		result.Refreshed = append(result.Refreshed, toEntry(e))
	}

	for _, e := range status.CancelledEntries() {
		result.Cancelled = append(result.Cancelled, toEntry(e))
	}

	for _, e := range status.RetriedEntries() {
		result.Retried = append(result.Retried, toEntry(e))
	}

	for _, e := range status.WaitingEntries() {
		result.Waiting = append(result.Waiting, toEntry(e))
	}

	for _, e := range status.ActionRequired() {
		if e.State == pr.StatusFailedSecurity {
			continue
		}
		result.ActionRequired = append(result.ActionRequired, toEntry(e))
	}

	for _, e := range status.EligibleEntries() {
		result.Eligible = append(result.Eligible, toEntry(e))
	}

	for _, e := range status.UnclassifiedEntries() {
		result.Unclassified = append(result.Unclassified, toEntry(e))
	}

	for _, e := range status.SkippedEntries() {
		result.Skipped = append(result.Skipped, toEntry(e))
	}

	return result
}

// groupUnhandled collects the unrecognised failures by signature, most
// frequent first, so a pattern worth a rule reads as a count.
func groupUnhandled(entries []pr.StatusEntry) []SweepUnhandled {
	bySignature := make(map[string]*SweepUnhandled)
	var order []string
	for _, e := range entries {
		group, seen := bySignature[e.Unhandled.Signature]
		if !seen {
			group = &SweepUnhandled{
				Signature: e.Unhandled.Signature,
				Checks:    e.Unhandled.Checks,
				Excerpt:   e.Unhandled.Excerpt,
			}
			bySignature[e.Unhandled.Signature] = group
			order = append(order, e.Unhandled.Signature)
		}
		group.Count++
		group.PRs = append(group.PRs, fmt.Sprintf("%s/%s#%d", e.PR.Owner, e.PR.Repo, e.PR.Number))
	}

	out := make([]SweepUnhandled, 0, len(order))
	for _, signature := range order {
		out = append(out, *bySignature[signature])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// groupFindings collects the repository settings that hold PRs back, by
// cause and then by repository, the cause on the most PRs first. Honey
// Badger sweeps 78 repositories, and a report with one line per repository
// does not reach a reader; a count per cause does.
func groupFindings(entries []pr.StatusEntry) []SweepFinding {
	counts := make(map[string]int)
	repos := make(map[string]map[string]*SweepFindingRepo)
	var order []string
	for _, e := range entries {
		cause := string(e.Finding.Cause)
		if _, seen := repos[cause]; !seen {
			repos[cause] = make(map[string]*SweepFindingRepo)
			order = append(order, cause)
		}
		counts[cause]++

		name := e.PR.Owner + "/" + e.PR.Repo
		repo, known := repos[cause][name]
		if !known {
			repo = &SweepFindingRepo{Repo: name, Detail: e.Finding.Detail}
			repos[cause][name] = repo
		}
		repo.PRs++
	}

	out := make([]SweepFinding, 0, len(order))
	for _, cause := range order {
		finding := SweepFinding{Cause: cause, PRs: counts[cause]}
		for _, repo := range repos[cause] {
			finding.Repositories = append(finding.Repositories, *repo)
		}
		sort.SliceStable(finding.Repositories, func(i, j int) bool {
			if finding.Repositories[i].PRs != finding.Repositories[j].PRs {
				return finding.Repositories[i].PRs > finding.Repositories[j].PRs
			}
			return finding.Repositories[i].Repo < finding.Repositories[j].Repo
		})
		out = append(out, finding)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PRs > out[j].PRs })
	return out
}
