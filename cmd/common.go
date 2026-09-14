package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
	"github.com/google/go-github/v91/github"
)

// RunOptions holds the configuration shared between the run and sweep commands.
type RunOptions struct {
	DryRun bool
	Watch  bool
	// NoTUI disables the live table and prints plain-text results to
	// stdout once processing finishes.
	NoTUI bool
	// Quiet suppresses every human-readable line: the live table and the
	// plain-text results on stdout as well as the progress and summary
	// lines on stderr. The caller reads the outcome from the returned
	// PRStatus instead. `marge serve` sets it because stdout is the MCP
	// stdio transport there and must carry nothing but JSON-RPC. Quiet
	// implies NoTUI.
	Quiet            bool
	Author           string
	TrustedAuthors   string
	MergeAuto        bool
	RefreshStale     bool
	RetryCancelled   bool
	Org              string
	ReposFile        string
	Grouping         string
	SecurityPatterns string
	Cols             []pr.TableColumn
}

func processOnceWithStatus(ctx context.Context, client *github.Client, login string, prs []pr.PRInfo, opts RunOptions) (*pr.PRStatus, error) {
	if len(prs) == 0 {
		if !opts.Quiet {
			fmt.Fprintln(os.Stderr, "No matching PRs found.")
		}
		return pr.NewPRStatus(), nil
	}

	if !opts.Quiet {
		fmt.Fprintf(os.Stderr, "Processing %d PR(s)...\n\n", len(prs))
	}

	sort.Slice(prs, func(i, j int) bool {
		ri := prs[i].Owner + "/" + prs[i].Repo
		rj := prs[j].Owner + "/" + prs[j].Repo
		if ri != rj {
			return ri < rj
		}
		return prs[i].Number < prs[j].Number
	})

	status := pr.NewPRStatus()
	indices := make([]int, len(prs))
	for i, p := range prs {
		indices[i] = status.Add(p)
	}

	cols := opts.Cols
	if cols == nil {
		cols = pr.FullColumns()
	}
	pr.AdjustColumnWidths(cols, prs)

	tui := !opts.NoTUI && !opts.Quiet

	if tui {
		pr.DisableLineWrap(os.Stdout)
		defer pr.EnableLineWrap(os.Stdout)

		pr.PrintTableHeader(os.Stdout, cols)
		for _, e := range status.Snapshot() {
			pr.PrintRow(os.Stdout, e, cols)
		}
	}

	stopRefresh := make(chan struct{})
	refreshStopped := make(chan struct{})
	if tui {
		go func() {
			defer close(refreshStopped)
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-stopRefresh:
					return
				case <-ticker.C:
					pr.UpdateTable(os.Stdout, status.Snapshot(), cols)
				}
			}
		}()
	} else {
		close(refreshStopped)
	}

	proc := process.NewProcessor(client, opts.DryRun, opts.MergeAuto, login, parseTrustedAuthors(opts.TrustedAuthors))
	proc.SecurityCheckPatterns = parseCSVList(opts.SecurityPatterns)
	proc.RefreshStale = opts.RefreshStale
	proc.RetryCancelled = opts.RetryCancelled
	proc.CircleCI = circleci.NewClient()

	// Build a per-repo index so we can look up each PR's status table index.
	indexByPR := make(map[string]int, len(prs))
	for i, p := range prs {
		key := fmt.Sprintf("%s/%s#%d", p.Owner, p.Repo, p.Number)
		indexByPR[key] = indices[i]
	}

	// Group PRs by owner/repo. PRs within the same repo are processed
	// sequentially to avoid "base branch was modified" failures. Different
	// repo groups run in parallel, bounded by the semaphore.
	repoGroups := pr.GroupByRepo(prs)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	for _, group := range repoGroups {
		wg.Add(1)
		go func(repoPRs []pr.PRInfo) {
			defer wg.Done()
			for _, info := range repoPRs {
				sem <- struct{}{}
				key := fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
				idx := indexByPR[key]
				proc.ProcessPR(ctx, info, status, idx)
				<-sem
			}
		}(group.PRs)
	}

	wg.Wait()

	close(stopRefresh)
	<-refreshStopped

	switch {
	case opts.Quiet:
		// Nothing is printed; the caller consumes the returned status.
	case opts.NoTUI:
		pr.PrintPlainResults(os.Stdout, status)
	default:
		pr.UpdateTable(os.Stdout, status.Snapshot(), cols)
		// Restore wrapping before any post-table prose so long lines
		// are not clipped by the disabled-wrap mode that protected the
		// table redraws.
		pr.EnableLineWrap(os.Stdout)
	}

	if !opts.Quiet {
		fmt.Fprintf(os.Stderr, "\n%s\n", status.FormatSummary())
	}

	return status, nil
}

func watchLoop(ctx context.Context, watch bool, fn func(ctx context.Context) error) error {
	for {
		if err := fn(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !watch {
			return nil
		}
		fmt.Fprintf(os.Stderr, "\nWaiting 60s before next poll... (Ctrl+C to stop)\n")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(60 * time.Second):
		}
	}
}

// parseCSVList splits a comma-separated string into trimmed, non-empty
// entries. It returns nil when the input has no usable entries so callers
// can distinguish "user did not configure this" from "user configured an
// explicit list".
func parseCSVList(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseTrustedAuthors(csv string) map[string]bool {
	entries := parseCSVList(csv)
	m := make(map[string]bool, len(entries))
	for _, a := range entries {
		m[a] = true
	}
	return m
}
