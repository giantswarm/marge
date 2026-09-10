package pr

import (
	"strings"
	"testing"
	"time"
)

func TestParseRescueMarker(t *testing.T) {
	body := `Rescue attempt failed: nock v14 is ESM-only and breaks Jest CJS resolution.

<!-- ai-rescue: {"tool":"klaus","outcome":"failed","reason":"ESM-only breaks Jest CJS","head_sha":"d9f00bf2","at":"2026-06-09T18:40:00Z"} -->`

	m := ParseRescueMarker(body)
	if m == nil {
		t.Fatal("ParseRescueMarker returned nil")
	}
	if m.Tool != "klaus" {
		t.Errorf("Tool = %q, want %q", m.Tool, "klaus")
	}
	if m.Outcome != "failed" {
		t.Errorf("Outcome = %q, want %q", m.Outcome, "failed")
	}
	if m.Reason != "ESM-only breaks Jest CJS" {
		t.Errorf("Reason = %q", m.Reason)
	}
	if m.HeadSHA != "d9f00bf2" {
		t.Errorf("HeadSHA = %q", m.HeadSHA)
	}
	want := time.Date(2026, 6, 9, 18, 40, 0, 0, time.UTC)
	if !m.At.Equal(want) {
		t.Errorf("At = %v, want %v", m.At, want)
	}
	if m.Comparable() {
		t.Errorf("legacy marker without patch_id/change_id parsed a fingerprint: %+v", m.Fingerprint)
	}
}

func TestParseRescueMarker_fingerprint(t *testing.T) {
	m := ParseRescueMarker(`<!-- ai-rescue: {"outcome":"blocked","head_sha":"1be1ed9d","patch_id":"5ad45e13d66acc2b","change_id":"typescript@v7"} -->`)
	if m == nil {
		t.Fatal("ParseRescueMarker returned nil")
	}
	if m.PatchID != "5ad45e13d66acc2b" || m.ChangeID != "typescript@v7" {
		t.Errorf("fingerprint = %+v", m.Fingerprint)
	}
}

func TestParseRescueMarker_noMarker(t *testing.T) {
	if m := ParseRescueMarker("just a regular comment"); m != nil {
		t.Errorf("expected nil, got %+v", m)
	}
}

func TestParseRescueMarker_malformedJSONIgnored(t *testing.T) {
	if m := ParseRescueMarker(`<!-- ai-rescue: {not json} -->`); m != nil {
		t.Errorf("expected nil for malformed JSON, got %+v", m)
	}
}

func TestParseRescueMarker_missingOutcomeIgnored(t *testing.T) {
	if m := ParseRescueMarker(`<!-- ai-rescue: {"tool":"klaus"} -->`); m != nil {
		t.Errorf("expected nil for marker without outcome, got %+v", m)
	}
}

func TestParseRescueMarker_lastMarkerWins(t *testing.T) {
	body := `<!-- ai-rescue: {"outcome":"failed","reason":"first"} -->
<!-- ai-rescue: {"outcome":"blocked","reason":"second"} -->`

	m := ParseRescueMarker(body)
	if m == nil {
		t.Fatal("ParseRescueMarker returned nil")
	}
	if m.Reason != "second" {
		t.Errorf("Reason = %q, want %q (last marker should win)", m.Reason, "second")
	}
}

func TestMarkStale(t *testing.T) {
	const (
		oldHead = "d9f00bf2d90e7dc6b3013a975c08d80766542745"
		newHead = "edf33f4d1cff7cab190b39689bd3ac77b6bd0910"
	)
	pinned := Fingerprint{PatchID: "5ad45e13d66acc2b", ChangeID: "typescript@v7"}

	tests := []struct {
		name        string
		marker      RescueMarker
		headSHA     string
		current     *Fingerprint // nil: no fingerprint can be computed now
		wantStale   bool
		wantRebased bool
	}{
		{"same full SHA", RescueMarker{HeadSHA: oldHead}, oldHead, nil, false, false},
		{"short marker SHA matches head prefix", RescueMarker{HeadSHA: oldHead[:8]}, oldHead, nil, false, false},
		{"marker without SHA never stale", RescueMarker{}, newHead, nil, false, false},
		{"unknown current head never stale", RescueMarker{HeadSHA: oldHead[:8]}, "", nil, false, false},
		{"head moved, legacy marker without fingerprint", RescueMarker{HeadSHA: oldHead[:8]}, newHead, &pinned, true, false},
		{"head moved, fingerprint unavailable now", RescueMarker{HeadSHA: oldHead, Fingerprint: pinned}, newHead, nil, true, false},
		{"rebased: same patch id", RescueMarker{HeadSHA: oldHead, Fingerprint: pinned}, newHead, &Fingerprint{PatchID: pinned.PatchID}, false, true},
		{"changed diff: other patch id", RescueMarker{HeadSHA: oldHead, Fingerprint: pinned}, newHead, &Fingerprint{PatchID: "0000000000000000", ChangeID: pinned.ChangeID}, true, false},
		{"patch id unavailable now, same change id", RescueMarker{HeadSHA: oldHead, Fingerprint: pinned}, newHead, &Fingerprint{ChangeID: "typescript@v7"}, false, true},
		{"version bump: other change id", RescueMarker{HeadSHA: oldHead, Fingerprint: Fingerprint{ChangeID: "typescript@v6"}}, newHead, &Fingerprint{PatchID: "abc", ChangeID: "typescript@v7"}, true, false},
		{"nothing comparable", RescueMarker{HeadSHA: oldHead, Fingerprint: Fingerprint{PatchID: "abc"}}, newHead, &Fingerprint{ChangeID: "x@v1"}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.marker
			m.Outcome = "failed"
			var current func() Fingerprint
			if tt.current != nil {
				fp := *tt.current
				current = func() Fingerprint { return fp }
			}
			m.MarkStale(tt.headSHA, current)
			if m.Stale != tt.wantStale || m.Rebased != tt.wantRebased {
				t.Errorf("Stale = %v, Rebased = %v; want %v, %v", m.Stale, m.Rebased, tt.wantStale, tt.wantRebased)
			}
		})
	}
}

func TestMarkStale_fingerprintComputedOnlyWhenNeeded(t *testing.T) {
	calls := 0
	current := func() Fingerprint { calls++; return Fingerprint{PatchID: "abc"} }

	sameHead := &RescueMarker{Outcome: "failed", HeadSHA: "d9f00bf2", Fingerprint: Fingerprint{PatchID: "abc"}}
	sameHead.MarkStale("d9f00bf2d90e7dc6b3013a975c08d80766542745", current)
	legacy := &RescueMarker{Outcome: "failed", HeadSHA: "d9f00bf2"}
	legacy.MarkStale("edf33f4d", current)
	if calls != 0 {
		t.Fatalf("fingerprint computed %d times for a same-head and a legacy marker, want 0", calls)
	}

	moved := &RescueMarker{Outcome: "failed", HeadSHA: "d9f00bf2", Fingerprint: Fingerprint{PatchID: "abc"}}
	moved.MarkStale("edf33f4d", current)
	if calls != 1 || !moved.Rebased || moved.Stale {
		t.Errorf("calls = %d, rebased = %v, stale = %v; want 1, true, false", calls, moved.Rebased, moved.Stale)
	}
}

func TestCommentBody_roundTrips(t *testing.T) {
	orig := &RescueMarker{
		Tool:        "klaus",
		Outcome:     "failed",
		Reason:      "pytest-helm-charts requires pytest<9",
		HeadSHA:     "d9f00bf2",
		At:          time.Date(2026, 6, 9, 18, 40, 0, 0, time.UTC),
		Fingerprint: Fingerprint{PatchID: "5ad45e13d66acc2b", ChangeID: "pytest@v9"},
	}

	body := orig.CommentBody()

	if !strings.Contains(body, "**AI rescue failed** (klaus): pytest-helm-charts requires pytest<9") {
		t.Errorf("human-readable part missing or wrong:\n%s", body)
	}
	if !strings.Contains(body, `"patch_id":"5ad45e13d66acc2b"`) || !strings.Contains(body, `"change_id":"pytest@v9"`) {
		t.Errorf("fingerprint missing from marker JSON:\n%s", body)
	}

	parsed := ParseRescueMarker(body)
	if parsed == nil {
		t.Fatal("CommentBody output did not parse back")
	}
	if parsed.Tool != orig.Tool || parsed.Outcome != orig.Outcome || parsed.Reason != orig.Reason || parsed.HeadSHA != orig.HeadSHA || !parsed.At.Equal(orig.At) || parsed.Fingerprint != orig.Fingerprint {
		t.Errorf("round-trip mismatch: got %+v, want %+v", parsed, orig)
	}
}

func TestCommentBody_omitsEmptyFingerprint(t *testing.T) {
	body := (&RescueMarker{Outcome: "failed", HeadSHA: "d9f00bf2"}).CommentBody()
	if strings.Contains(body, "patch_id") || strings.Contains(body, "change_id") {
		t.Errorf("marker without fingerprint must not serialise empty fingerprint fields:\n%s", body)
	}
}

func TestFormatRescue(t *testing.T) {
	now := time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)

	fresh := &RescueMarker{Tool: "klaus", Outcome: "failed", Reason: "ESM-only", At: now.Add(-25 * time.Hour)}
	if got := FormatRescue(fresh, now); got != "rescue failed 1d ago (klaus): ESM-only" {
		t.Errorf("fresh = %q", got)
	}

	stale := &RescueMarker{Tool: "klaus", Outcome: "failed", Reason: "ESM-only", At: now.Add(-25 * time.Hour), Stale: true}
	if got := FormatRescue(stale, now); got != "rescue failed 1d ago (klaus), stale: new commits since" {
		t.Errorf("stale = %q", got)
	}

	rebased := &RescueMarker{Tool: "klaus", Outcome: "blocked", Reason: "peer typescript <6.1.0", At: now.Add(-5 * 24 * time.Hour), Rebased: true}
	if got := FormatRescue(rebased, now); got != "rescue blocked 5d ago (klaus), rebased since: same change: peer typescript <6.1.0" {
		t.Errorf("rebased = %q", got)
	}
	if got := ColorizeRescue(rebased, now); !strings.HasPrefix(got, "\033[1;91m[") {
		t.Errorf("a rebased-but-valid marker must render like a fresh one (bold red), got %q", got)
	}

	if got := FormatRescue(nil, now); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
}
