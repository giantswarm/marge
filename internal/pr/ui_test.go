package pr

import (
	"testing"
	"unicode/utf8"
)

func TestFitColumnWidths(t *testing.T) {
	prs := []PRInfo{
		{Owner: "giantswarm", Repo: "substrate-credential-providers", Title: "fix(deps): update module github.com/giantswarm/substrate to v1.17.1", Author: "renovate[bot]"},
		{Owner: "giantswarm", Repo: "kagent-upstream", Title: "sync: re-pin onto upstream 7a0a4fd blocked — ci(fork): test and scan the consumed branch, re-pin weekly by workflow, keep a ledger", Author: "heraldbot[bot]"},
	}

	t.Run("a long title narrows to the terminal", func(t *testing.T) {
		cols := FullColumns()
		AdjustColumnWidths(cols, prs)
		repoWidth := cols[0].Width
		FitColumnWidths(cols, 160)
		if got := tableWidth(cols); got != 160 {
			t.Errorf("table width = %d, want 160", got)
		}
		if cols[0].Width != repoWidth {
			t.Errorf("repository width = %d, want %d: only the widest column narrows", cols[0].Width, repoWidth)
		}
	})

	t.Run("a table that fits stays as it is", func(t *testing.T) {
		cols := FullColumns()
		AdjustColumnWidths(cols, prs)
		want := tableWidth(cols)
		FitColumnWidths(cols, want)
		if got := tableWidth(cols); got != want {
			t.Errorf("table width = %d, want %d", got, want)
		}
	})

	t.Run("an unknown terminal leaves the widths", func(t *testing.T) {
		cols := FullColumns()
		AdjustColumnWidths(cols, prs)
		want := tableWidth(cols)
		FitColumnWidths(cols, 0)
		if got := tableWidth(cols); got != want {
			t.Errorf("table width = %d, want %d", got, want)
		}
	})

	t.Run("no column narrows below its label", func(t *testing.T) {
		cols := FullColumns()
		AdjustColumnWidths(cols, prs)
		FitColumnWidths(cols, 20)
		for _, c := range cols {
			if c.Width < len(c.Label) {
				t.Errorf("%s width = %d, below its label", c.Label, c.Width)
			}
		}
	})
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"fits", "typescript", 10, "typescript"},
		{"cuts with an ellipsis", "typescript", 8, "types..."},
		{"too short for an ellipsis", "typescript", 3, "typ"},
		{"never splits a rune", "blocked — ci(fork)", 10, "blocked..."},
		{"keeps a multi-byte rune whole", "blocked — ci(fork)", 12, "blocked —..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) = %q, not valid UTF-8", tt.s, tt.max, got)
			}
		})
	}
}
