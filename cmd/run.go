package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/google/go-github/v92/github"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
)

var runOpts RunOptions

var runActions string

func init() {
	runCmd.Flags().BoolVar(&runOpts.DryRun, "dry-run", false, "Show what would be done without making changes")
	runCmd.Flags().StringVar(&runActions, "actions", "", "Comma-separated sweep steps to run, in fixed order: "+strings.Join(process.ActionNames(), ", ")+" (default: all)")
	runCmd.Flags().BoolVarP(&runOpts.Watch, "watch", "w", false, "Keep polling for new PRs (every 60s)")
	runCmd.Flags().StringVar(&runOpts.Grouping, "grouping", "repo", "Group by \"repo\" or \"dependency\"")
	runCmd.Flags().StringVar(&runOpts.Org, "org", "", "Limit to repos owned by this org or user")
	runCmd.Flags().StringVar(&runOpts.ReposFile, "repos-file", "", "File with org/repo entries (one per line) to scan for bot PRs instead of searching GitHub")
	runCmd.Flags().BoolVar(&runOpts.NoTUI, "no-tui", false, "Disable live table, print plain-text results instead")
	runCmd.Flags().BoolVar(&runOpts.MergeAuto, "merge-auto", false, "Also merge PRs that have auto-merge enabled")
	runCmd.Flags().StringVar(&runOpts.SecurityPatterns, "security-patterns", "", "Comma-separated case-insensitive substrings added to the built-in list that flags failing CI checks as security-related")

	rootCmd.AddCommand(runCmd)

	rootCmd.RunE = runCmd.RunE
	rootCmd.Args = cobra.MaximumNArgs(1)
	rootCmd.Flags().AddFlagSet(runCmd.Flags())
}

var runCmd = &cobra.Command{
	Use:   "run [query]",
	Short: "Find, approve, and merge bot PRs interactively",
	Long: `Search for open bot PRs (Renovate, Align files, Herald, Dependabot)
requesting your review, optionally group them interactively, then approve
and merge the eligible green ones.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		actions, err := process.ParseActions(runActions)
		if err != nil {
			return err
		}
		runOpts.Actions = actions
		runOpts.CheckTimeout = interactiveCheckTimeout

		client, err := gh.NewClient(ctx)
		if err != nil {
			return err
		}

		me, _, err := client.Users.Get(ctx, "")
		if err != nil {
			return fmt.Errorf("getting authenticated user: %w", err)
		}
		login := me.GetLogin()

		query := ""
		if len(args) > 0 {
			query = args[0]
		}

		return watchLoop(ctx, runOpts.Watch, func(ctx context.Context) error {
			repos, err := runOpts.repoList(ctx, client)
			if err != nil {
				return err
			}

			found, err := searchPRs(ctx, client, query, login, repos)
			if err != nil {
				return fmt.Errorf("searching PRs: %w", err)
			}
			for _, f := range found.Failed {
				fmt.Fprintf(os.Stderr, "repository %s not listed: %s\n", f.Repo, f.Err)
			}
			prs := filterByOrg(found.PRs, runOpts.Org)

			opts := runOpts
			opts.Cols = pr.FullColumns()

			if query == "" && len(prs) > 0 {
				selected, specificGroup, err := interactiveSelect(prs, runOpts.Grouping)
				if err != nil {
					return err
				}
				prs = selected

				if len(prs) == 0 {
					fmt.Fprintln(os.Stderr, "No PRs selected.")
					return nil
				}

				if specificGroup {
					switch runOpts.Grouping {
					case "repo":
						opts.Cols = pr.RepoSelectedColumns()
					case "dependency":
						opts.Cols = pr.DependencySelectedColumns()
					}
				}
			}

			_, err = processOnceWithStatus(ctx, client, login, prs, opts)
			return err
		})
	},
}

// discovery is what a PR search found: the bot PRs to process and the
// repositories whose PRs could not be listed. A repository that fails to
// list is reported, never silently dropped from the sweep.
type discovery struct {
	PRs    []pr.PRInfo
	Failed []repoFailure
}

type repoFailure struct {
	Repo string
	Err  string
}

// searchPRs finds the open bot PRs to process. With a repo list
// ("owner/name" entries) it lists the bot PRs of exactly those
// repositories and query is a case-insensitive substring filter on the
// repository names; without one it runs the GitHub search, where query
// becomes part of the search string. Only PRs by the four trusted bots
// are returned.
func searchPRs(ctx context.Context, client *github.Client, query string, login string, repos []string) (discovery, error) {
	if len(repos) > 0 {
		return listRepoPRs(ctx, client, repos, query)
	}

	scopeFilters := []string{
		"review-requested:@me",
		fmt.Sprintf("user:%s", login),
	}

	seen := make(map[string]bool)
	var found discovery

	for _, scope := range scopeFilters {
		for _, botLogin := range pr.TrustedLogins() {
			authorFilter := "author:app/" + strings.TrimSuffix(botLogin, "[bot]")
			searchQuery := fmt.Sprintf("%s is:pr is:open archived:false %s %s", query, scope, authorFilter)
			searchQuery = strings.TrimSpace(searchQuery)

			opts := &github.SearchOptions{
				Sort:        "updated",
				ListOptions: github.ListOptions{PerPage: 100},
			}

			for {
				result, resp, err := client.Search.Issues(ctx, searchQuery, opts)
				if err != nil {
					return discovery{}, fmt.Errorf("search failed: %w", err)
				}

				for _, issue := range result.Issues {
					url := issue.GetHTMLURL()
					if seen[url] {
						continue
					}
					seen[url] = true

					owner, repo, err := pr.ExtractOwnerRepo(url)
					if err != nil {
						continue
					}

					found.PRs = append(found.PRs, pr.PRInfo{
						Owner:     owner,
						Repo:      repo,
						Number:    issue.GetNumber(),
						Title:     issue.GetTitle(),
						URL:       url,
						Author:    issue.GetUser().GetLogin(),
						CreatedAt: issue.GetCreatedAt().Time,
					})
				}

				if resp.NextPage == 0 {
					break
				}
				opts.Page = resp.NextPage
			}
		}
	}

	return found, nil
}

func listRepoPRs(ctx context.Context, client *github.Client, repos []string, query string) (discovery, error) {
	type repoRef struct{ Owner, Name string }
	var refs []repoRef
	queryLower := strings.ToLower(query)
	for _, entry := range repos {
		owner, name, ok := strings.Cut(strings.TrimSpace(entry), "/")
		if !ok {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(owner+"/"+name), queryLower) {
			continue
		}
		refs = append(refs, repoRef{owner, name})
	}

	var (
		mu    sync.Mutex
		seen  = make(map[string]bool)
		found discovery
		wg    sync.WaitGroup
	)
	sem := make(chan struct{}, 10)

	for _, ref := range refs {
		wg.Add(1)
		go func(owner, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			opts := &github.PullRequestListOptions{
				State:       "open",
				Sort:        "updated",
				ListOptions: github.ListOptions{PerPage: 100},
			}
			var batch []pr.PRInfo
			for {
				pulls, resp, err := client.PullRequests.List(ctx, owner, name, opts)
				if err != nil {
					mu.Lock()
					found.Failed = append(found.Failed, repoFailure{Repo: owner + "/" + name, Err: err.Error()})
					mu.Unlock()
					return
				}

				for _, pull := range pulls {
					author := pull.GetUser().GetLogin()
					if pr.KindOf(author) == "" {
						continue
					}
					batch = append(batch, pr.PRInfo{
						Owner:     owner,
						Repo:      name,
						Number:    pull.GetNumber(),
						Title:     pull.GetTitle(),
						URL:       pull.GetHTMLURL(),
						Author:    author,
						CreatedAt: pull.GetCreatedAt().Time,
						BaseRef:   pull.GetBase().GetRef(),
					})
				}

				if resp.NextPage == 0 {
					break
				}
				opts.Page = resp.NextPage
			}

			mu.Lock()
			for _, p := range batch {
				if !seen[p.URL] {
					seen[p.URL] = true
					found.PRs = append(found.PRs, p)
				}
			}
			mu.Unlock()
		}(ref.Owner, ref.Name)
	}

	wg.Wait()
	sort.Slice(found.Failed, func(i, j int) bool { return found.Failed[i].Repo < found.Failed[j].Repo })
	return found, nil
}

func interactiveSelect(prs []pr.PRInfo, grouping string) ([]pr.PRInfo, bool, error) {
	var groups []pr.PRGroup
	switch grouping {
	case "dependency":
		groups = pr.GroupByDependency(prs)
	default:
		groups = pr.GroupByRepo(prs)
	}

	items := make([]string, 0, len(groups)+1)
	items = append(items, fmt.Sprintf("All (%d PRs)", len(prs)))
	for _, g := range groups {
		authors := uniqueAuthors(g.PRs)
		items = append(items, fmt.Sprintf("%s (%d PRs) [%s]", g.Key, g.Count, strings.Join(authors, ", ")))
	}

	prompt := promptui.Select{
		Label: "Select PRs to process",
		Items: items,
		Size:  20,
	}

	idx, _, err := prompt.Run()
	if err != nil {
		return nil, false, fmt.Errorf("selection cancelled: %w", err)
	}

	if idx == 0 {
		return prs, false, nil
	}

	return groups[idx-1].PRs, true, nil
}

func uniqueAuthors(prs []pr.PRInfo) []string {
	seen := make(map[string]bool)
	var authors []string
	for _, p := range prs {
		if p.Author != "" && !seen[p.Author] {
			seen[p.Author] = true
			authors = append(authors, p.Author)
		}
	}
	return authors
}
