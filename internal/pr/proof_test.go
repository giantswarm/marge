package pr

import "testing"

// renovateBody is the tail every Renovate PR carries: a checklist that asks
// for nothing before the merge.
const renovateBody = `### Configuration

📅 **Schedule**: Branch creation - At any time.

🚦 **Automerge**: Disabled by config. Please merge this manually once you are satisfied.

---

 - [ ] <!-- rebase-check -->If you want to rebase/retry this PR, check this box
`

func TestUnprovenBeforeMerge(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantItem string
		wantOpen bool
	}{
		{name: "empty body"},
		{name: "renovate rebase box", body: renovateBody},
		{
			name:     "heading with an open item",
			body:     "Bumps the chart.\n\n## Before merging\n\n- [x] CI green\n- [ ] Proven in the lab on a pre-step agent\n",
			wantItem: "Proven in the lab on a pre-step agent",
			wantOpen: true,
		},
		{
			name: "heading with every item ticked",
			body: "## Before merging\n\n- [x] CI green\n- [X] Proven in the lab\n",
		},
		{
			name:     "sentence then a list after a paragraph",
			body:     "Before merging this, prove:\n\nThe migration must keep every conversation.\n\n* [ ] migration run on old data\n",
			wantItem: "migration run on old data",
			wantOpen: true,
		},
		{
			name:     "the item says it itself",
			body:     "Summary.\n\n- [ ] Run the e2e proof before merge\n",
			wantItem: "Run the e2e proof before merge",
			wantOpen: true,
		},
		{
			name: "open item under another heading",
			body: "## Before merging\n\n- [x] proven\n\n## Follow-ups\n\n- [ ] clean up later\n",
		},
		{
			name: "open item after a closing paragraph",
			body: "Before merging:\n- [x] proven\n\nThat is all.\n\n- [ ] unrelated todo\n",
		},
		{
			name: "phrase alone without a checklist",
			body: "Please review before merging.\n",
		},
		{
			name: "merge commit wording is not the phrase",
			body: "- [ ] merged before the release\n",
		},
		{
			name:     "numbered items and CRLF",
			body:     "Before merging:\r\n1. [x] lint\r\n2. [ ] agentlab proof\r\n",
			wantItem: "agentlab proof",
			wantOpen: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, open := UnprovenBeforeMerge(tt.body)
			if open != tt.wantOpen || item != tt.wantItem {
				t.Fatalf("UnprovenBeforeMerge() = (%q, %v), want (%q, %v)", item, open, tt.wantItem, tt.wantOpen)
			}
		})
	}
}
