package pr

import (
	"strings"
	"testing"
)

func TestStatusFailedSecurity_string(t *testing.T) {
	if got := StatusFailedSecurity.String(); got != "Failed (security)" {
		t.Errorf("StatusFailedSecurity.String() = %q, want %q", got, "Failed (security)")
	}
}

func TestStatus_securityFailureCountedAsFailed(t *testing.T) {
	s := NewPRStatus()
	idx := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 1})
	s.Update(idx, StatusFailedSecurity, "security check failed: govulncheck")

	c := s.Summary()
	if c.Failed != 1 {
		t.Errorf("Summary failed = %d, want 1", c.Failed)
	}
	if c.Merged != 0 || c.Blocked != 0 || c.Skipped != 0 {
		t.Errorf("Summary merged=%d blocked=%d skipped=%d, want zero", c.Merged, c.Blocked, c.Skipped)
	}
}

func TestStatus_securityFailureInActionRequired(t *testing.T) {
	s := NewPRStatus()
	idx := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 7})
	s.Update(idx, StatusFailedSecurity, "security check failed: trivy")

	ar := s.ActionRequired()
	if len(ar) != 1 {
		t.Fatalf("ActionRequired len = %d, want 1", len(ar))
	}
	if ar[0].State != StatusFailedSecurity {
		t.Errorf("entry state = %v, want StatusFailedSecurity", ar[0].State)
	}

	sec := s.SecurityFailedEntries()
	if len(sec) != 1 {
		t.Fatalf("SecurityFailedEntries len = %d, want 1", len(sec))
	}
}

func TestStatusBlockedCI_string(t *testing.T) {
	if got := StatusBlockedCI.String(); got != "CI unavailable (budget)" {
		t.Errorf("StatusBlockedCI.String() = %q, want %q", got, "CI unavailable (budget)")
	}
}

func TestStatus_blockedNotCountedAsFailed(t *testing.T) {
	s := NewPRStatus()
	idx := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 1})
	s.Update(idx, StatusBlockedCI, "Actions budget exhausted; no jobs ran")

	c := s.Summary()
	if c.Blocked != 1 {
		t.Errorf("Summary blocked = %d, want 1", c.Blocked)
	}
	if c.Failed != 0 {
		t.Errorf("Summary failed = %d, want 0 (budget block must not count as failed)", c.Failed)
	}
	if c.Merged != 0 || c.Skipped != 0 {
		t.Errorf("Summary merged=%d skipped=%d, want zero", c.Merged, c.Skipped)
	}
}

func TestStatus_blockedNotInActionRequired(t *testing.T) {
	s := NewPRStatus()
	idx := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 42})
	s.Update(idx, StatusBlockedCI, "Actions budget exhausted; no jobs ran: Test, Lint")

	if ar := s.ActionRequired(); len(ar) != 0 {
		t.Errorf("ActionRequired len = %d, want 0 (budget block must be kept out of the rescue path)", len(ar))
	}

	blocked := s.BlockedEntries()
	if len(blocked) != 1 {
		t.Fatalf("BlockedEntries len = %d, want 1", len(blocked))
	}
	if blocked[0].PR.Number != 42 {
		t.Errorf("BlockedEntries[0].Number = %d, want 42", blocked[0].PR.Number)
	}
}

func TestFormatSummary_includesBlockedWhenPresent(t *testing.T) {
	s := NewPRStatus()
	idx := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 1})
	s.Update(idx, StatusBlockedCI, "Actions budget exhausted; no jobs ran")

	if got := s.FormatSummary(); !strings.Contains(got, "CI-unavailable") {
		t.Errorf("FormatSummary() = %q, want it to mention CI-unavailable", got)
	}
}

func TestColorizeStatus_blockedIsDistinct(t *testing.T) {
	failed := ColorizeStatus(StatusFailed, "checks failed")
	blocked := ColorizeStatus(StatusBlockedCI, "Actions budget exhausted")
	skipped := ColorizeStatus(StatusSkipped, "dry-run")

	if blocked == failed {
		t.Error("ColorizeStatus for StatusBlockedCI and StatusFailed must differ")
	}
	if blocked == skipped {
		t.Error("ColorizeStatus for StatusBlockedCI and StatusSkipped must differ")
	}
}

func TestColorizeStatus_securityIsDistinct(t *testing.T) {
	regular := ColorizeStatus(StatusFailed, "checks failed")
	security := ColorizeStatus(StatusFailedSecurity, "govulncheck")

	if regular == security {
		t.Fatal("ColorizeStatus for StatusFailed and StatusFailedSecurity must differ")
	}
	if !strings.Contains(security, "security") {
		t.Errorf("security colorize output %q should contain the word 'security'", security)
	}
}

func TestStatusStale_strings(t *testing.T) {
	if got := StatusStale.String(); got != "Stale" {
		t.Errorf("StatusStale.String() = %q, want %q", got, "Stale")
	}
	if got := StatusRefreshed.String(); got != "Refreshed" {
		t.Errorf("StatusRefreshed.String() = %q, want %q", got, "Refreshed")
	}
}

func TestStatus_staleAndRefreshedAreNotFailures(t *testing.T) {
	s := NewPRStatus()
	i1 := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 1})
	s.Update(i1, StatusStale, "go-build green on main since 2026-09-05 10:57 UTC, 3 behind")
	i2 := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 2})
	s.Update(i2, StatusRefreshed, "re-checking; go-build green on main")
	i3 := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 3})
	s.Update(i3, StatusFailed, "checks failed: go-build")

	c := s.Summary()
	if c.Failed != 1 || c.Stale != 1 || c.Refreshed != 1 {
		t.Errorf("Summary = %+v, want failed=1 stale=1 refreshed=1", c)
	}
	if ar := s.ActionRequired(); len(ar) != 1 || ar[0].PR.Number != 3 {
		t.Errorf("ActionRequired = %+v, want only #3 (stale/refreshed stay out of the rescue path)", ar)
	}
	if st := s.StaleEntries(); len(st) != 1 || st[0].PR.Number != 1 {
		t.Errorf("StaleEntries = %+v, want only #1", st)
	}
	if rf := s.RefreshedEntries(); len(rf) != 1 || rf[0].PR.Number != 2 {
		t.Errorf("RefreshedEntries = %+v, want only #2", rf)
	}
	summary := s.FormatSummary()
	for _, want := range []string{"1 failed", "1 stale", "1 refreshed"} {
		if !strings.Contains(summary, want) {
			t.Errorf("FormatSummary() = %q, want it to contain %q", summary, want)
		}
	}
}

func TestStatus_rescueAt(t *testing.T) {
	s := NewPRStatus()
	idx := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 1})
	if s.RescueAt(idx) != nil {
		t.Fatal("RescueAt should be nil before SetRescue")
	}
	m := &RescueMarker{Outcome: "failed"}
	s.SetRescue(idx, m)
	if s.RescueAt(idx) != m {
		t.Error("RescueAt should return the attached marker")
	}
	if s.RescueAt(99) != nil {
		t.Error("RescueAt out of range should be nil")
	}
}

func TestColorizeStatus_staleAndRefreshedAreNotRed(t *testing.T) {
	failed := ColorizeStatus(StatusFailed, "x")
	stale := ColorizeStatus(StatusStale, "x")
	refreshed := ColorizeStatus(StatusRefreshed, "x")
	if stale == failed || refreshed == failed {
		t.Error("stale and refreshed must not render in the failure color")
	}
}

func TestStatusCancelled_strings(t *testing.T) {
	if got := StatusCancelled.String(); got != "Cancelled" {
		t.Errorf("StatusCancelled.String() = %q, want %q", got, "Cancelled")
	}
	if got := StatusRetried.String(); got != "Retried" {
		t.Errorf("StatusRetried.String() = %q, want %q", got, "Retried")
	}
}

func TestStatus_cancelledAndRetriedAreNotFailures(t *testing.T) {
	s := NewPRStatus()
	i1 := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 1})
	s.Update(i1, StatusCancelled, "build 1263 auto-cancelled; retry needed")
	i2 := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 2})
	s.Update(i2, StatusRetried, "re-checking; build 1263 retried as 1272")
	i3 := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 3})
	s.Update(i3, StatusFailed, "checks failed: ci/circleci: go-build")

	c := s.Summary()
	if c.Failed != 1 || c.Cancelled != 1 || c.Retried != 1 {
		t.Errorf("Summary = %+v, want failed=1 cancelled=1 retried=1", c)
	}
	if ar := s.ActionRequired(); len(ar) != 1 || ar[0].PR.Number != 3 {
		t.Errorf("ActionRequired = %+v, want only #3 (cancelled/retried stay out of the rescue path)", ar)
	}
	if ce := s.CancelledEntries(); len(ce) != 1 || ce[0].PR.Number != 1 {
		t.Errorf("CancelledEntries = %+v, want only #1", ce)
	}
	if re := s.RetriedEntries(); len(re) != 1 || re[0].PR.Number != 2 {
		t.Errorf("RetriedEntries = %+v, want only #2", re)
	}
	summary := s.FormatSummary()
	for _, want := range []string{"1 failed", "1 cancelled", "1 retried"} {
		if !strings.Contains(summary, want) {
			t.Errorf("FormatSummary() = %q, want it to contain %q", summary, want)
		}
	}
}

func TestColorizeStatus_cancelledAndRetriedAreNotRed(t *testing.T) {
	failed := ColorizeStatus(StatusFailed, "x")
	cancelled := ColorizeStatus(StatusCancelled, "x")
	retried := ColorizeStatus(StatusRetried, "x")
	if cancelled == failed || retried == failed {
		t.Error("cancelled and retried must not render in the failure color")
	}
	if cancelled == retried {
		t.Error("a build awaiting a retry and one re-running must look different")
	}
}

// TestStatus_awaitingRebaseIsNotActionRequired keeps a conflict the sweep
// caused itself out of the list a person reads. The bot rebases the PR.
func TestStatus_awaitingRebaseIsNotActionRequired(t *testing.T) {
	s := NewPRStatus()
	idx := s.Add(PRInfo{Owner: "o", Repo: "r", Number: 7})
	s.Update(idx, StatusAwaitingRebase, "conflicted by #6, merged in this run; the bot rebases it")

	if got := s.ActionRequired(); len(got) != 0 {
		t.Errorf("ActionRequired len = %d, want 0", len(got))
	}
	c := s.Summary()
	if c.Failed != 0 || c.Waiting != 1 {
		t.Errorf("Summary failed=%d waiting=%d, want 0 and 1", c.Failed, c.Waiting)
	}
	if got := s.WaitingEntries(); len(got) != 1 {
		t.Errorf("WaitingEntries len = %d, want 1: the count and the list must agree", len(got))
	}
	if got := LabelClass(StatusAwaitingRebase); got != "pending" {
		t.Errorf("LabelClass = %q, want %q", got, "pending")
	}
}
