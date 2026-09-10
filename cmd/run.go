package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/google/go-github/v91/github"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	gh "github.com/teemow/marge/internal/github"
	"github.com/teemow/marge/internal/pr"
)

var runOpts RunOptions

func init() {
	runCmd.Flags().BoolVar(&runOpts.DryRun, "dry-run", false, "Show what would be done without making changes")
	runCmd.Flags().BoolVarP(&runOpts.Watch, "watch", "w", false, "Keep polling for new PRs (every 60s)")
	runCmd.Flags().StringVar(&runOpts.Grouping, "grouping", "repo", "Group by \"repo\" or \"dependency\"")
	runCmd.Flags().StringVar(&runOpts.Author, "author", "all", "Filter by PR author: \"renovate\", \"dependabot\", or \"all\"")
	runCmd.Flags().StringVar(&runOpts.Org, "org", "", "Limit to repos owned by this org or user")
	runCmd.Flags().StringVar(&runOpts.ReposFile, "repos-file", "", "File with org/repo entries (one per line) to also scan for bot PRs")
	runCmd.Flags().BoolVar(&runOpts.NoTUI, "no-tui", false, "Disable live table, print plain-text results instead")
	runCmd.Flags().BoolVar(&runOpts.MergeAuto, "merge-auto", false, "Also merge PRs that have auto-merge enabled")
	runCmd.Flags().BoolVar(&runOpts.RefreshStale, "refresh-stale", false, "Update the branch of stale PRs (behind base, failing checks green on base) so CI re-runs")
	runCmd.Flags().BoolVar(&runOpts.RetryCancelled, "retry-cancelled", false, "Retry CircleCI builds that CircleCI auto-cancelled on the PR head so the same commit gets a real verdict")
	runCmd.Flags().StringVar(&runOpts.TrustedAuthors, "trusted-authors", "renovate[bot],dependabot[bot]", "Comma-separated list of trusted PR author logins")
	runCmd.Flags().StringVar(&runOpts.SecurityPatterns, "security-patterns", "", "Comma-separated list of case-insensitive substrings used to flag failing CI checks as security-related (defaults to a built-in list)")

	rootCmd.AddCommand(runCmd)

	rootCmd.RunE = runCmd.RunE
	rootCmd.Args = cobra.MaximumNArgs(1)
	rootCmd.Flags().AddFlagSet(runCmd.Flags())
}

var runCmd = &cobra.Command{
	Use:   "run [query]",
	Short: "Find, approve, and merge dependency update PRs",
	Long: `Search for open Renovate and Dependabot PRs requesting your review,
optionally group them interactively, then approve and merge them.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

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
			prs, err := searchPRs(ctx, client, query, login, runOpts.Author, runOpts.ReposFile)
			if err != nil {
				return fmt.Errorf("searching PRs: %w", err)
			}

			if runOpts.Org != "" {
				filtered := prs[:0]
				for _, p := range prs {
					if strings.EqualFold(p.Owner, runOpts.Org) {
						filtered = append(filtered, p)
					}
				}
				prs = filtered
			}

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

func searchPRs(ctx context.Context, client *github.Client, query string, login string, authorFilter string, reposFile string) ([]pr.PRInfo, error) {
	if reposFile != "" {
		seen := make(map[string]bool)
		return listRepoPRs(ctx, client, reposFile, query, authorFilter, seen)
	}

	var authorFilters []string
	switch authorFilter {
	case "renovate":
		authorFilters = []string{"author:app/renovate"}
	case "dependabot":
		authorFilters = []string{"author:app/dependabot"}
	default:
		authorFilters = []string{"author:app/renovate", "author:app/dependabot"}
	}

	scopeFilters := []string{
		"review-requested:@me",
		fmt.Sprintf("user:%s", login),
	}

	seen := make(map[string]bool)
	var allPRs []pr.PRInfo

	for _, scope := range scopeFilters {
		for _, af := range authorFilters {
			searchQuery := fmt.Sprintf("%s is:pr is:open archived:false %s %s", query, scope, af)
			searchQuery = strings.TrimSpace(searchQuery)

			opts := &github.SearchOptions{
				Sort:        "updated",
				ListOptions: github.ListOptions{PerPage: 100},
			}

			for {
				result, resp, err := client.Search.Issues(ctx, searchQuery, opts)
				if err != nil {
					return nil, fmt.Errorf("search failed: %w", err)
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

					allPRs = append(allPRs, pr.PRInfo{
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

	selfQuery := fmt.Sprintf("%s is:pr is:open archived:false user:%s author:%s", query, login, login)
	selfQuery = strings.TrimSpace(selfQuery)

	selfOpts := &github.SearchOptions{
		Sort:        "updated",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		result, resp, err := client.Search.Issues(ctx, selfQuery, selfOpts)
		if err != nil {
			return nil, fmt.Errorf("search failed: %w", err)
		}

		for _, issue := range result.Issues {
			url := issue.GetHTMLURL()
			if seen[url] {
				continue
			}

			title := issue.GetTitle()
			if !pr.IsDependencyUpdateTitle(title) {
				continue
			}

			seen[url] = true

			owner, repo, err := pr.ExtractOwnerRepo(url)
			if err != nil {
				continue
			}

			allPRs = append(allPRs, pr.PRInfo{
				Owner:     owner,
				Repo:      repo,
				Number:    issue.GetNumber(),
				Title:     title,
				URL:       url,
				Author:    issue.GetUser().GetLogin(),
				CreatedAt: issue.GetCreatedAt().Time,
			})
		}

		if resp.NextPage == 0 {
			break
		}
		selfOpts.Page = resp.NextPage
	}

	return allPRs, nil
}

func listRepoPRs(ctx context.Context, client *github.Client, reposFile, query, authorFilter string, seen map[string]bool) ([]pr.PRInfo, error) {
	data, err := os.ReadFile(reposFile)
	if err != nil {
		return nil, fmt.Errorf("reading repos file: %w", err)
	}

	botAuthors := make(map[string]bool)
	switch authorFilter {
	case "renovate":
		botAuthors["renovate[bot]"] = true
	case "dependabot":
		botAuthors["dependabot[bot]"] = true
	default:
		botAuthors["renovate[bot]"] = true
		botAuthors["dependabot[bot]"] = true
	}

	type repoRef struct{ Owner, Name string }
	var repos []repoRef
	queryLower := strings.ToLower(query)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "/", 2)
		if len(parts) != 2 {
			continue
		}
		owner, name := parts[0], parts[1]
		if query != "" && !strings.Contains(strings.ToLower(owner+"/"+name), queryLower) {
			continue
		}
		repos = append(repos, repoRef{owner, name})
	}

	var (
		mu     sync.Mutex
		allPRs []pr.PRInfo
		wg     sync.WaitGroup
	)
	sem := make(chan struct{}, 10)

	for _, repo := range repos {
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
			for {
				pulls, resp, err := client.PullRequests.List(ctx, owner, name, opts)
				if err != nil {
					return
				}

				var batch []pr.PRInfo
				for _, pull := range pulls {
					url := pull.GetHTMLURL()
					author := pull.GetUser().GetLogin()
					if !botAuthors[author] {
						continue
					}
					batch = append(batch, pr.PRInfo{
						Owner:     owner,
						Repo:      name,
						Number:    pull.GetNumber(),
						Title:     pull.GetTitle(),
						URL:       url,
						Author:    author,
						CreatedAt: pull.GetCreatedAt().Time,
					})
				}

				mu.Lock()
				for _, p := range batch {
					if !seen[p.URL] {
						seen[p.URL] = true
						allPRs = append(allPRs, p)
					}
				}
				mu.Unlock()

				if resp.NextPage == 0 {
					break
				}
				opts.Page = resp.NextPage
			}
		}(repo.Owner, repo.Name)
	}

	wg.Wait()
	return allPRs, nil
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
