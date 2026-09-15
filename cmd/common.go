package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
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
	Quiet     bool
	MergeAuto bool
	// Team scopes the sweep to the repositories listed in that team's file
	// in giantswarm/github. Empty means the query scope.
	Team string
	// Query is the GitHub search text of the query scope.
	Query string
	// Actions selects the sweep steps; see process.ParseActions.
	Actions process.ActionSet
	// CheckTimeout is how long one PR waits for pending checks; zero means
	// no wait.
	CheckTimeout     time.Duration
	Org              string
	ReposFile        string // repositories to scan instead of searching GitHub; see repoList
	Grouping         string
	SecurityPatterns string
	Cols             []pr.TableColumn
}

// repoList returns the repositories a run is restricted to: the team's
// repositories under the team scope, else the entries of ReposFile when
// one was given. Nil means no restriction, so the PRs come from the GitHub
// search.
func (o RunOptions) repoList(ctx context.Context, client *github.Client) ([]string, error) {
	if o.Team != "" {
		return teamRepos(ctx, client, o.Team)
	}
	if o.ReposFile == "" {
		return nil, nil
	}
	return readReposFile(o.ReposFile)
}

// teamFileOwner and teamFileRepo name the repository that holds one file
// per team listing the repositories the team owns.
const (
	teamFileOwner = "giantswarm"
	teamFileRepo  = "github"
)

// teamRepos resolves a team's repositories from repositories/team-<name>.yaml
// in giantswarm/github. Only each entry's name is read; every other key of
// the team file belongs to the generators and changes without notice.
func teamRepos(ctx context.Context, client *github.Client, team string) ([]string, error) {
	path := fmt.Sprintf("repositories/team-%s.yaml", team)
	file, _, resp, err := client.Repositories.GetContents(ctx, teamFileOwner, teamFileRepo, path, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("no team file for %q: %s/%s has no %s", team, teamFileOwner, teamFileRepo, path)
		}
		return nil, fmt.Errorf("reading team file %s: %w", path, err)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decoding team file %s: %w", path, err)
	}
	return parseTeamFile(content, path)
}

// parseTeamFile returns the owner/name entries of a team file's content.
func parseTeamFile(content, path string) ([]string, error) {
	var entries []struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal([]byte(content), &entries); err != nil {
		return nil, fmt.Errorf("parsing team file %s: %w", path, err)
	}
	var repos []string
	for _, e := range entries {
		if name := strings.TrimSpace(e.Name); name != "" {
			repos = append(repos, teamFileOwner+"/"+name)
		}
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("team file %s lists no repositories", path)
	}
	return repos, nil
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

	proc := process.NewProcessor(client, opts.DryRun, opts.MergeAuto, login)
	proc.SecurityCheckPatterns = parseCSVList(opts.SecurityPatterns)
	proc.Actions = opts.Actions
	proc.CheckTimeout = opts.CheckTimeout
	proc.CircleCI = circleci.NewClient()
	// Cross-PR knowledge, so it is computed once from the whole list before
	// the per-PR processing starts, and read without locking afterwards.
	proc.SupersededBy = pr.FindSuperseded(prs)

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
	for p := range strings.SplitSeq(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readReposFile returns the "owner/name" entries listed in the file at
// path, one per line and trimmed. Blank lines and lines starting with #
// are ignored. A file that lists no repository is an error: the caller
// would otherwise fall back to searching all of GitHub, which is never
// what a repos file asks for.
func readReposFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading repos file: %w", err)
	}
	var repos []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		repos = append(repos, line)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("repos file %s lists no repositories", path)
	}
	return repos, nil
}

// filterByOrg keeps the PRs whose owner is org, compared case-insensitively
// like GitHub does. An empty org keeps every PR.
func filterByOrg(prs []pr.PRInfo, org string) []pr.PRInfo {
	if org == "" {
		return prs
	}
	var filtered []pr.PRInfo
	for _, p := range prs {
		if strings.EqualFold(p.Owner, org) {
			filtered = append(filtered, p)
		}
	}
	return filtered
}
