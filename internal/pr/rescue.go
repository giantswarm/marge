package pr

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RescueMarker records a prior automated rescue attempt on a PR. It is
// embedded as a machine-readable HTML comment inside an ordinary PR
// comment, so any tool that can comment on a PR (a klaus agent, a Cursor
// agent, a GitHub Action) can participate without coupling to marge:
//
//	<!-- ai-rescue: {"tool":"klaus","outcome":"failed","reason":"...","head_sha":"...","at":"2026-06-09T18:40:00Z"} -->
//
// HeadSHA ties the attempt to the code it was attempted against. Renovate
// force-pushes on rebase or version change, so a marker whose head_sha no
// longer matches the PR head describes code that may no longer exist. The
// embedded Fingerprint (patch_id, change_id; optional, absent from markers
// written before it existed) tells the two apart: when the head moved but
// the fingerprint still matches, the branch was only rebased and the
// attempt still stands; otherwise it is stale and the PR is fair game for
// another rescue.
type RescueMarker struct {
	Tool    string `json:"tool,omitempty"`
	Outcome string `json:"outcome"`
	// Kind tells a rescue record ("" or "rescue") from a sweep evidence
	// record ("evidence"). Evidence never counts as a prior rescue attempt.
	Kind    string    `json:"kind,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	HeadSHA string    `json:"head_sha,omitempty"`
	At      time.Time `json:"at,omitempty"`
	Fingerprint

	// Signature, Checks and Excerpt describe a failure no rule of the
	// catalogue recognised. They are set on a marker whose Outcome is
	// MarkerOutcomeUnhandled, and carry what `marge rules draft` needs to
	// build a skeleton from the PR alone. Signature groups the same failure
	// across PRs.
	Signature string   `json:"signature,omitempty"`
	Checks    []string `json:"checks,omitempty"`
	Excerpt   string   `json:"excerpt,omitempty"`

	// Stale and Rebased are computed by MarkStale, never serialized into
	// the marker. Rebased is set when the head moved but the change did not.
	Stale   bool `json:"-"`
	Rebased bool `json:"-"`
}

var rescueMarkerRE = regexp.MustCompile(`(?s)<!--\s*ai-rescue:\s*(\{.*?\})\s*-->`)

// MarkerKindEvidence marks a comment the sweep wrote as evidence of a guard
// decision or an action it performed. Evidence carries the head SHA and the
// fingerprint like a rescue marker so a later sweep can tell whether the
// same evidence already stands for the current change.
const MarkerKindEvidence = "evidence"

// MarkerOutcomeUnhandled is the outcome of an evidence marker that records
// a failure no rule recognised. It names no action, because none ran: the
// marker is the only record that the failure happened, and the sweep's
// process keeps none.
const MarkerOutcomeUnhandled = "unhandled"

// UnhandledPhrase is the literal the unhandled marker's human line carries,
// so a search over PR comments finds every PR that holds one.
const UnhandledPhrase = "unrecognised failure"

// IsUnhandled reports whether the marker records a failure no rule
// recognised, with the signature that groups it across PRs.
func (m *RescueMarker) IsUnhandled() bool {
	return m.IsEvidence() && m.Outcome == MarkerOutcomeUnhandled && m.Signature != ""
}

// IsEvidence reports whether the marker is sweep evidence rather than the
// record of a rescue attempt.
func (m *RescueMarker) IsEvidence() bool {
	return m.Kind == MarkerKindEvidence
}

// ParseRescueMarker extracts the last ai-rescue marker from a comment
// body. It returns nil when the body contains no parseable marker; a
// malformed JSON payload is ignored rather than treated as an error so a
// mangled comment can never break a sweep.
func ParseRescueMarker(body string) *RescueMarker {
	matches := rescueMarkerRE.FindAllStringSubmatch(body, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		var m RescueMarker
		if err := json.Unmarshal([]byte(matches[i][1]), &m); err != nil {
			continue
		}
		if m.Outcome == "" {
			continue
		}
		return &m
	}
	return nil
}

// MarkStale sets Stale and Rebased by comparing the marker with the PR's
// current head. Same head (short-vs-full SHA prefixes match): fresh. Head
// moved: the marker stays fresh, flagged Rebased, when the change it was
// written for is still the change on the branch -- decided by the marker's
// fingerprint against the current one, which `current` computes on demand.
// It is only called when the head moved and the marker carries a
// fingerprint; nil means none can be computed. Otherwise the marker is
// stale. A marker without a recorded SHA cannot be aged out, so it stays
// fresh until a newer marker replaces it.
func (m *RescueMarker) MarkStale(currentHeadSHA string, current func() Fingerprint) {
	m.Stale, m.Rebased = false, false
	if m.HeadSHA == "" || currentHeadSHA == "" || shaPrefixMatch(m.HeadSHA, currentHeadSHA) {
		return
	}
	if m.Comparable() && current != nil {
		m.Rebased = m.Matches(current())
	}
	m.Stale = !m.Rebased
}

func shaPrefixMatch(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if len(a) > len(b) {
		a, b = b, a
	}
	return a != "" && strings.HasPrefix(b, a)
}

// CommentBody renders the full PR comment for this marker: a
// human-readable summary followed by the machine-readable marker.
func (m *RescueMarker) CommentBody() string {
	tool := m.Tool
	if tool == "" {
		tool = "ai"
	}
	human := fmt.Sprintf("**AI rescue %s** (%s)", m.Outcome, tool)
	switch {
	case m.IsUnhandled():
		human = fmt.Sprintf("**Sweep: %s** (%s), signature %s", UnhandledPhrase, tool, m.Signature)
	case m.IsEvidence():
		human = fmt.Sprintf("**Sweep: %s** (%s)", m.Outcome, tool)
	}
	if m.Reason != "" {
		human += ": " + m.Reason
	}

	payload, _ := json.Marshal(m)
	return fmt.Sprintf("%s\n\n<!-- ai-rescue: %s -->", human, payload)
}

// FormatRescue renders the marker as a short human-readable annotation
// for status output, e.g. "rescue failed 1d ago (klaus): ESM-only",
// "rescue failed 3d ago (klaus), stale: new commits since" or
// "rescue blocked 5d ago (klaus), rebased since: same change: ESM-only".
func FormatRescue(m *RescueMarker, now time.Time) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("rescue ")
	b.WriteString(m.Outcome)
	if !m.At.IsZero() {
		b.WriteString(" ")
		b.WriteString(FormatAge(m.At, now))
		b.WriteString(" ago")
	}
	if m.Tool != "" {
		fmt.Fprintf(&b, " (%s)", m.Tool)
	}
	if m.Stale {
		b.WriteString(", stale: new commits since")
		return b.String()
	}
	if m.Rebased {
		b.WriteString(", rebased since: same change")
	}
	if m.Reason != "" {
		b.WriteString(": ")
		b.WriteString(m.Reason)
	}
	return b.String()
}

// ColorizeRescue wraps the FormatRescue annotation in ANSI colors: a
// stale marker renders yellow (retriable -- the change differs from the
// one attempted), a fresh one bold red (a rescue already failed on exactly
// this change, whether or not the branch was rebased since; a human is
// needed).
func ColorizeRescue(m *RescueMarker, now time.Time) string {
	s := FormatRescue(m, now)
	if s == "" {
		return ""
	}
	if m.Stale {
		return fmt.Sprintf("\033[33m[%s]\033[0m", s)
	}
	return fmt.Sprintf("\033[1;91m[%s]\033[0m", s)
}
