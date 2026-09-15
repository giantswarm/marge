package pr

import "testing"

func botPR(number int, title string) PRInfo {
	return PRInfo{Owner: "giantswarm", Repo: "agentgateway", Number: number, Title: title, Author: "renovate[bot]"}
}

func TestFindSuperseded_higherSiblingWins(t *testing.T) {
	// giantswarm/agentgateway, 2026-08-25: #29 moved actions/checkout to
	// v5.1.0 while #30 moved it to v7.0.1. Only #30 is worth merging.
	prs := []PRInfo{
		botPR(29, "chore(deps): update actions/checkout action to v5.1.0"),
		botPR(30, "chore(deps): update actions/checkout action to v7.0.1"),
	}

	got := FindSuperseded(prs)
	if len(got) != 1 {
		t.Fatalf("superseded = %v, want exactly #29", got)
	}
	detail, ok := got[PRKey(prs[0])]
	if !ok {
		t.Fatalf("#29 is not marked superseded: %v", got)
	}
	if detail != "superseded by #30 (v7.0.1)" {
		t.Errorf("detail = %q", detail)
	}
}

func TestFindSuperseded_threeSiblingsLeaveOnlyTheHighest(t *testing.T) {
	prs := []PRInfo{
		botPR(29, "chore(deps): update actions/checkout action to v5.1.0"),
		botPR(30, "chore(deps): update actions/checkout action to v7.0.1"),
		botPR(31, "chore(deps): update actions/checkout action to v6.2.0"),
	}

	got := FindSuperseded(prs)
	if len(got) != 2 {
		t.Fatalf("superseded = %v, want #29 and #31", got)
	}
	if _, ok := got[PRKey(prs[1])]; ok {
		t.Error("#30 carries the highest version and must not be superseded")
	}
}

func TestFindSuperseded_needsPositiveEvidence(t *testing.T) {
	tests := []struct {
		name string
		prs  []PRInfo
	}{
		{
			name: "a lone PR has no sibling",
			prs:  []PRInfo{botPR(29, "chore(deps): update actions/checkout action to v5.1.0")},
		},
		{
			name: "different dependencies are not siblings",
			prs: []PRInfo{
				botPR(29, "chore(deps): update actions/checkout action to v5.1.0"),
				botPR(30, "chore(deps): update actions/setup-go action to v7.0.1"),
			},
		},
		{
			name: "equal versions supersede nothing",
			prs: []PRInfo{
				botPR(29, "chore(deps): update actions/checkout action to v5.1.0"),
				botPR(30, "chore(deps): update actions/checkout action to v5.1.0"),
			},
		},
		{
			name: "a title without a version is not compared",
			prs: []PRInfo{
				botPR(29, "chore(deps): update actions/checkout action"),
				botPR(30, "chore(deps): update actions/checkout action to v7.0.1"),
			},
		},
		{
			name: "a grouped update names no single version",
			prs: []PRInfo{
				botPR(29, "chore(deps): update all non-major dependencies"),
				botPR(30, "chore(deps): update all non-major dependencies"),
			},
		},
		{
			name: "the same dependency in two repositories",
			prs: []PRInfo{
				{Owner: "giantswarm", Repo: "a", Number: 1, Title: "chore(deps): update actions/checkout action to v5.1.0"},
				{Owner: "giantswarm", Repo: "b", Number: 2, Title: "chore(deps): update actions/checkout action to v7.0.1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FindSuperseded(tt.prs); len(got) != 0 {
				t.Errorf("superseded = %v, want none", got)
			}
		})
	}
}

func TestFindSuperseded_dependabotFromToTitles(t *testing.T) {
	prs := []PRInfo{
		{Owner: "giantswarm", Repo: "marge", Number: 7, Title: "Bump github.com/google/go-github/v92 from 92.0.0 to 92.1.0"},
		{Owner: "giantswarm", Repo: "marge", Number: 8, Title: "Bump github.com/google/go-github/v92 from 92.0.0 to 93.0.0"},
	}

	got := FindSuperseded(prs)
	if detail := got[PRKey(prs[0])]; detail != "superseded by #8 (93.0.0)" {
		t.Fatalf("superseded = %v, want #7 superseded by #8", got)
	}
}

func TestFindSuperseded_differentBaseBranchesAreNotSiblings(t *testing.T) {
	// A maintenance branch pins the lower version on purpose, so the PR
	// against main must not replace it.
	prs := []PRInfo{
		{Owner: "o", Repo: "r", Number: 1, BaseRef: "release-1.x", Title: "chore(deps): update actions/checkout action to v5.1.0"},
		{Owner: "o", Repo: "r", Number: 2, BaseRef: "main", Title: "chore(deps): update actions/checkout action to v7.0.1"},
	}

	if got := FindSuperseded(prs); len(got) != 0 {
		t.Errorf("superseded = %v, want none", got)
	}
}
