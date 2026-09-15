package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/policy"
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
	CheckTimeout time.Duration
	// Policies is the sweep policy of the scope, resolved from the policy
	// files by resolveScope before the sweep starts.
	Policies         *policy.Set
	Org              string
	ReposFile        string // repositories to scan instead of searching GitHub; see resolveScope
	Grouping         string
	SecurityPatterns string
	Cols             []pr.TableColumn
}

// resolveScope reads the policy files and returns what the run covers: the
// repositories it is restricted to and the policy they are swept under. The
// team scope takes both from the team's files; every other scope takes the
// repositories from ReposFile, or nothing, which leaves the PRs to the
// GitHub search, and the policy from the company default file alone.
//
// It runs at the start of every sweep, so a policy change takes effect on
// the next run without a restart.
func (o RunOptions) resolveScope(ctx context.Context, client *github.Client) (policy.Scope, error) {
	loader, err := policyLoader(client)
	if err != nil {
		return policy.Scope{}, err
	}
	if o.Team != "" {
		return loader.TeamScope(ctx, o.Team)
	}
	scope, err := loader.QueryScope(ctx)
	if err != nil {
		return policy.Scope{}, err
	}
	if o.ReposFile == "" {
		return scope, nil
	}
	repos, err := readReposFile(o.ReposFile)
	if err != nil {
		return policy.Scope{}, err
	}
	scope.Repos = repos
	return scope, nil
}

// policyLoader returns a loader over the repository that holds the team
// files and the policy files.
func policyLoader(client *github.Client) (policy.Loader, error) {
	owner, name, err := teamFileRepo()
	if err != nil {
		return policy.Loader{}, err
	}
	source := policy.GitHubSource{Client: client, Owner: owner, Repo: name}
	return policy.Loader{Source: source, Owner: owner}, nil
}

// teamFileRepoEnv names the owner/repo that holds one file per team
// listing the repositories the team owns; defaultTeamFileRepo applies when
// it is unset.
const (
	teamFileRepoEnv     = "MARGE_TEAM_FILE_REPO"
	defaultTeamFileRepo = "giantswarm/github"
)

// teamFileRepo returns the owner and name of the team-file repository.
func teamFileRepo() (owner, name string, err error) {
	spec := strings.TrimSpace(os.Getenv(teamFileRepoEnv))
	if spec == "" {
		spec = defaultTeamFileRepo
	}
	owner, name, ok := strings.Cut(spec, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%s=%q: want owner/repo", teamFileRepoEnv, spec)
	}
	return owner, name, nil
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
	proc.Policies = opts.Policies
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

	// Group PRs by owner/repo, then fan out under the policy's concurrency.
	forEachPR(pr.GroupByRepo(prs), opts.Policies.Base().Concurrency, func(info pr.PRInfo) {
		key := fmt.Sprintf("%s/%s#%d", info.Owner, info.Repo, info.Number)
		proc.ProcessPR(ctx, info, status, indexByPR[key])
	})

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
		reportUnenforced(os.Stderr, opts.Policies.Base())
	}

	return status, nil
}

// forEachPR runs fn on every PR of every group, under the two bounds the
// policy declares: perTeam repositories run at once, and within one
// repository perRepo PRs. A repository holds its slot for as long as it has
// work, so perTeam bounds repositories and not the PRs they hold together.
// The default perRepo of 1 keeps the PRs of a repository sequential, which
// is what avoids "base branch was modified" failures.
func forEachPR(groups []pr.PRGroup, concurrency pr.Concurrency, fn func(pr.PRInfo)) {
	var wg sync.WaitGroup
	repoSlots := make(chan struct{}, max(concurrency.PerTeam, 1))

	for _, group := range groups {
		wg.Add(1)
		go func(repoPRs []pr.PRInfo) {
			defer wg.Done()
			repoSlots <- struct{}{}
			defer func() { <-repoSlots }()

			var repoWG sync.WaitGroup
			prSlots := make(chan struct{}, max(concurrency.PerRepo, 1))
			for _, info := range repoPRs {
				prSlots <- struct{}{}
				repoWG.Add(1)
				go func(info pr.PRInfo) {
					defer repoWG.Done()
					defer func() { <-prSlots }()
					fn(info)
				}(info)
			}
			repoWG.Wait()
		}(group.PRs)
	}

	wg.Wait()
}

// reportUnenforced names the caps the policy declares that this build does
// not enforce. A cap a team wrote down and nothing applies must be said out
// loud, or the team reads the file as a guarantee.
func reportUnenforced(w io.Writer, resolved pr.Policy) {
	unenforced := resolved.DeclaredUnenforced()
	if len(unenforced) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "policy declares %s; this build does not enforce it yet\n", strings.Join(unenforced, " and "))
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
