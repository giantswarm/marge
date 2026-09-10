package cmd

import (
	"testing"

	"github.com/teemow/marge/internal/pr"
)

// TestBuildSweepResult_failedAndSecurityAreDisjoint guards the contract
// documented on SweepSummary: a security failure must be counted in
// SecurityFailures and not in Failed, so consumers can sum them without
// double-counting.
func TestBuildSweepResult_failedAndSecurityAreDisjoint(t *testing.T) {
	status := pr.NewPRStatus()
	idx1 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx1, pr.StatusFailed, "checks failed")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusFailedSecurity, "security check failed: trivy")
	idx3 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 3})
	status.Update(idx3, pr.StatusMerged, "squash")
	idx4 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 4})
	status.Update(idx4, pr.StatusSkipped, "dry-run")

	got := buildSweepResult(status)

	if got.Summary.Total != 4 {
		t.Errorf("Total = %d, want 4", got.Summary.Total)
	}
	if got.Summary.Merged != 1 {
		t.Errorf("Merged = %d, want 1", got.Summary.Merged)
	}
	if got.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (security failures must be excluded)", got.Summary.Failed)
	}
	if got.Summary.SecurityFailures != 1 {
		t.Errorf("SecurityFailures = %d, want 1", got.Summary.SecurityFailures)
	}
	if got.Summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", got.Summary.Skipped)
	}

	if len(got.SecurityFailures) != 1 {
		t.Fatalf("SecurityFailures slice len = %d, want 1", len(got.SecurityFailures))
	}
	if got.SecurityFailures[0].Number != 2 {
		t.Errorf("SecurityFailures[0].Number = %d, want 2", got.SecurityFailures[0].Number)
	}

	if len(got.ActionRequired) != 1 {
		t.Fatalf("ActionRequired slice len = %d, want 1", len(got.ActionRequired))
	}
	if got.ActionRequired[0].Number != 1 {
		t.Errorf("ActionRequired[0].Number = %d, want 1", got.ActionRequired[0].Number)
	}
}

// TestBuildSweepResult_ciUnavailableIsSeparate guards that a PR blocked by a
// GitHub Actions budget is reported under ci_unavailable and excluded from
// both the failed count and action_required, so rescue tooling never picks
// it up.
func TestBuildSweepResult_ciUnavailableIsSeparate(t *testing.T) {
	status := pr.NewPRStatus()
	idx1 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx1, pr.StatusFailed, "checks failed: build")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusBlockedCI, "Actions budget exhausted; no jobs ran: Test, Lint")

	got := buildSweepResult(status)

	if got.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (budget block must be excluded)", got.Summary.Failed)
	}
	if got.Summary.CIUnavailable != 1 {
		t.Errorf("CIUnavailable = %d, want 1", got.Summary.CIUnavailable)
	}
	if len(got.CIUnavailable) != 1 {
		t.Fatalf("CIUnavailable slice len = %d, want 1", len(got.CIUnavailable))
	}
	if got.CIUnavailable[0].Number != 2 {
		t.Errorf("CIUnavailable[0].Number = %d, want 2", got.CIUnavailable[0].Number)
	}
	for _, e := range got.ActionRequired {
		if e.Number == 2 {
			t.Error("budget-blocked PR must not appear in action_required")
		}
	}
}

// TestBuildSweepResult_staleAndRefreshedAreSeparate guards that a stale
// failure (fixed on the base branch already) and a refreshed branch are
// reported under their own keys and excluded from failed/action_required,
// so rescue tooling never dispatches an agent for a branch that only
// needs a refresh.
func TestBuildSweepResult_staleAndRefreshedAreSeparate(t *testing.T) {
	status := pr.NewPRStatus()
	idx1 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx1, pr.StatusFailed, "checks failed: build")
	idx2 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 2})
	status.Update(idx2, pr.StatusStale, "go-build green on main since 2026-09-05 10:57 UTC, 5 behind")
	idx3 := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 3})
	status.Update(idx3, pr.StatusRefreshed, "re-checking; go-build green on main since 2026-09-05 10:57 UTC, 5 behind")

	got := buildSweepResult(status)

	if got.Summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (stale/refreshed must be excluded)", got.Summary.Failed)
	}
	if got.Summary.Stale != 1 || got.Summary.Refreshed != 1 {
		t.Errorf("Stale = %d, Refreshed = %d, want 1 and 1", got.Summary.Stale, got.Summary.Refreshed)
	}
	if len(got.Stale) != 1 || got.Stale[0].Number != 2 || got.Stale[0].Status != "Stale" {
		t.Errorf("Stale = %+v, want only #2 with status Stale", got.Stale)
	}
	if len(got.Refreshed) != 1 || got.Refreshed[0].Number != 3 || got.Refreshed[0].Status != "Refreshed" {
		t.Errorf("Refreshed = %+v, want only #3 with status Refreshed", got.Refreshed)
	}
	if len(got.ActionRequired) != 1 || got.ActionRequired[0].Number != 1 {
		t.Errorf("ActionRequired = %+v, want only #1", got.ActionRequired)
	}
}

// TestBuildSweepResult_rescueRebased guards that the rescue object tells a
// marker whose branch was merely rebased (still valid) apart from a stale
// one, so orchestrators keep skipping the rebased PR.
func TestBuildSweepResult_rescueRebased(t *testing.T) {
	status := pr.NewPRStatus()
	idx := status.Add(pr.PRInfo{Owner: "o", Repo: "r", Number: 1})
	status.Update(idx, pr.StatusFailed, "checks failed: build")
	status.SetRescue(idx, &pr.RescueMarker{Tool: "klaus", Outcome: "blocked", Reason: "peer dep", Rebased: true})

	got := buildSweepResult(status)

	if len(got.ActionRequired) != 1 || got.ActionRequired[0].Rescue == nil {
		t.Fatalf("ActionRequired = %+v, want one entry with a rescue object", got.ActionRequired)
	}
	rescue := got.ActionRequired[0].Rescue
	if rescue.Stale || !rescue.Rebased {
		t.Errorf("rescue = %+v, want stale=false rebased=true", rescue)
	}
}
