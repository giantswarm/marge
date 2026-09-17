package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/logs"
	"github.com/giantswarm/marge/internal/patterns"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
)

var rulesFlags struct {
	path string
	repo string
	ref  string
}

func init() {
	for _, cmd := range []*cobra.Command{rulesValidateCmd, rulesTestCmd, rulesDraftCmd} {
		cmd.Flags().StringVar(&rulesFlags.path, "path", "rules", "Directory holding the catalogue")
	}
	rulesValidateCmd.Flags().StringVar(&rulesFlags.repo, "repo", "", "Read the catalogue from this repository instead, as owner/name")
	rulesValidateCmd.Flags().StringVar(&rulesFlags.ref, "ref", "", "Branch to read the catalogue from with --repo")

	rulesCmd.AddCommand(rulesValidateCmd, rulesTestCmd)
	rootCmd.AddCommand(rulesCmd)
}

var rulesCmd = &cobra.Command{
	Use:   "rules",
	Short: "Work with the rule catalogue the sweep loads at runtime",
	Long: `The sweep reads its rule catalogue from giantswarm/marge at the start of
every run, so a merged rule is live on the next run without a release. These
commands are what CI runs before a rule may merge.`,
	Args: cobra.NoArgs,
}

var rulesValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Check that every document of the catalogue is a usable rule",
	Long: `Decode and validate every document of the catalogue. A document that fails
here is skipped at runtime, so the rule it carries would silently not exist.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		catalogue, err := loadCatalogueForCLI(cmd.Context())
		if err != nil {
			return err
		}
		for _, skipped := range catalogue.Skipped {
			fmt.Fprintf(os.Stderr, "%s: %s\n", skipped.Path, skipped.Reason)
		}
		if len(catalogue.Skipped) > 0 {
			return fmt.Errorf("%d of %d documents are not usable rules", len(catalogue.Skipped), len(catalogue.Skipped)+len(catalogue.Rules))
		}
		registry := remedy.Default()
		fmt.Printf("%d rules, catalogue %s\n", len(catalogue.Rules), catalogue.Digest)
		for _, rule := range catalogue.Rules {
			fmt.Printf("  %-34s %-24s %s\n", rule.Name, rule.Action.Name, guardSet(registry, rule))
		}
		return nil
	},
}

var rulesTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Replay every scenario against the catalogue",
	Long: `Replay the recorded scenarios under rules/testdata against the catalogue and
report every rule that matched something it should not, or nothing where it
should have. A rule needs one scenario that matches it and one that does not,
so a rule cannot land on a signal nobody recorded.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		catalogue, err := loadCatalogueForCLI(cmd.Context())
		if err != nil {
			return err
		}
		scenarios, err := rules.LoadScenarios(filepath.Join(rulesFlags.path, rules.ScenarioDir))
		if err != nil {
			return err
		}

		failures := 0
		for _, scenario := range scenarios {
			if problem := scenario.Run(catalogue); problem != "" {
				failures++
				fmt.Fprintf(os.Stderr, "%s: %s\n", scenario.Path, problem)
			}
		}

		coverage := rules.CheckCoverage(catalogue, scenarios)
		for _, name := range coverage.NoPositive {
			failures++
			fmt.Fprintf(os.Stderr, "rule %s has no scenario that matches it\n", name)
		}
		for _, name := range coverage.NoNegative {
			failures++
			fmt.Fprintf(os.Stderr, "rule %s has no scenario that refuses it\n", name)
		}
		for _, name := range coverage.Orphans {
			failures++
			fmt.Fprintf(os.Stderr, "scenario directory %s names no rule of the catalogue\n", name)
		}
		if failures > 0 {
			return fmt.Errorf("%d scenario problems", failures)
		}
		fmt.Printf("%d scenarios over %d rules\n", len(scenarios), len(catalogue.Rules))
		return nil
	},
}

// guardSet is what refuses the rule's action: the guards the action itself
// enforces, then the refusals the rule adds. A held action says so first,
// because it refuses before any guard runs.
func guardSet(registry *remedy.Registry, rule *rules.Rule) string {
	if held := registry.HeldReason(rule.Action.Name); held != "" {
		return "HELD: " + held
	}
	names := registry.GuardNames(rule.Action.Name)
	for _, refusal := range rule.Refuse {
		names = append(names, "+"+refusal)
	}
	return strings.Join(names, " ")
}

// loadCatalogueForCLI reads the catalogue from disk, or from a repository
// when --repo asks for it.
func loadCatalogueForCLI(ctx context.Context) (*rules.Catalogue, error) {
	if rulesFlags.ref != "" && rulesFlags.repo == "" {
		return nil, fmt.Errorf("--ref names a branch of a repository: give --repo too, or drop --ref to read %s", rulesFlags.path)
	}
	loader := rules.Loader{LocalPath: rulesFlags.path, Ref: rulesFlags.ref}
	if rulesFlags.repo != "" {
		owner, name, found := strings.Cut(rulesFlags.repo, "/")
		if !found {
			return nil, fmt.Errorf("--repo takes %q, got %q", "owner/name", rulesFlags.repo)
		}
		client, err := gh.NewClient(ctx)
		if err != nil {
			return nil, err
		}
		loader.Client, loader.Owner, loader.Repo, loader.LocalPath = client, owner, name, ""
	}
	return loader.Load(ctx, remedy.Default())
}

var draftFlags struct {
	from string
	org  string
	name string
	repo string
	base string
	noPR bool
}

func init() {
	rulesDraftCmd.Flags().StringVar(&draftFlags.from, "from", "", "Sweep report to read the signature from; - reads standard input. Unset reads the markers the sweep left on the pull requests")
	rulesDraftCmd.Flags().StringVar(&draftFlags.org, "org", rules.DefaultOwner, "Organization whose pull requests carry the markers, read when --from is unset")
	rulesDraftCmd.Flags().StringVar(&draftFlags.name, "name", "", "Name of the rule to draft (default: derived from the failing checks)")
	rulesDraftCmd.Flags().StringVar(&draftFlags.repo, "repo", rules.DefaultOwner+"/"+rules.DefaultRepo, "Repository to open the draft pull request against, as owner/name")
	rulesDraftCmd.Flags().StringVar(&draftFlags.base, "base", rules.DefaultRef, "Branch the draft pull request is opened against")
	rulesDraftCmd.Flags().BoolVar(&draftFlags.noPR, "no-pr", false, "Write the files only; open no pull request")
	rulesCmd.AddCommand(rulesDraftCmd)
}

var rulesDraftCmd = &cobra.Command{
	Use:   "draft <signature>",
	Short: "Draft a rule and its scenarios from an unrecognised failure",
	Long: `Read one unhandled signature and write a rule skeleton with a pair of
scenarios built from the PRs that carry it, then open a draft pull request
carrying the same files.

The signature comes from the markers the sweep leaves on the pull requests it
could not handle, which is what this command reads by default. --from reads a
saved sweep report (marge sweep --output json) instead.

The skeleton leaves the action blank on purpose: it does not validate until a
person names one, so promoting a pattern is editing a draft rather than
writing one from nothing. Edit the files and push them to the branch the
command reports.

The pull request is opened through the API, so no git workspace and no push
credential are needed; the token needs contents:write and pull-requests:write
on the repository. --no-pr writes the files and stops, and prints the git and
gh commands instead.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		group, err := signatureGroup(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		name := draftFlags.name
		if name == "" {
			name = ruleNameFor(group)
		}
		files := buildDraft(name, group)
		if err := writeDraft(name, files); err != nil {
			return err
		}
		for _, f := range files {
			fmt.Println("wrote", f.path)
		}
		fmt.Printf("\nName the action in %s, then run: marge rules validate && marge rules test\n", files[0].path)

		if draftFlags.noPR {
			fmt.Println("\nOpen the draft pull request with:")
			fmt.Printf("  git checkout -b %s && git add %s && git commit && gh pr create --draft\n",
				draftBranch(name), rulesFlags.path)
			return nil
		}

		owner, repo, found := strings.Cut(draftFlags.repo, "/")
		if !found {
			return fmt.Errorf("--repo takes %q, got %q", "owner/name", draftFlags.repo)
		}
		client, err := gh.NewClient(cmd.Context())
		if err != nil {
			return err
		}
		pull, err := openDraftPR(cmd.Context(), client, owner, repo, draftFlags.base, name, files, group)
		if err != nil {
			return fmt.Errorf("%w\n\nThe files are written. Rerun with --no-pr for the commands that open the pull request by hand", err)
		}
		fmt.Printf("\ndraft pull request: %s\n", pull.GetHTMLURL())
		fmt.Printf("push your edits to %s\n", draftBranch(name))
		return nil
	},
}

// signatureGroup finds one signature, in the sweep report --from names or,
// with no --from, in the markers the sweep left on the pull requests of the
// organization.
func signatureGroup(ctx context.Context, signature string) (*SweepUnhandled, error) {
	if draftFlags.from != "" {
		report, err := readSweepReport(draftFlags.from)
		if err != nil {
			return nil, err
		}
		return findSignature(report, signature)
	}

	client, err := gh.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	search := patterns.Search{Client: client, Org: draftFlags.org}
	group, err := search.Signature(ctx, signature)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, fmt.Errorf("no pull request of %s carries signature %q; run `marge rules signatures` for the ones that were seen", draftFlags.org, signature)
	}
	return fromGroup(group), nil
}

// fromGroup is the group as the draft reads it. The two shapes say the same
// thing, one read from a run and one read from the pull requests.
func fromGroup(group *patterns.Group) *SweepUnhandled {
	return &SweepUnhandled{
		Signature: group.Signature,
		Checks:    group.Checks,
		Count:     group.Count,
		PRs:       group.PRs,
		Excerpt:   group.Excerpt,
	}
}

func readSweepReport(from string) (*SweepResult, error) {
	var raw []byte
	var err error
	if from == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(from)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the sweep report: %w", err)
	}
	var report SweepResult
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("the sweep report is not the JSON of `marge sweep --output json`: %w", err)
	}
	return &report, nil
}

func findSignature(report *SweepResult, signature string) (*SweepUnhandled, error) {
	for i := range report.Unhandled {
		if report.Unhandled[i].Signature == signature {
			return &report.Unhandled[i], nil
		}
	}
	known := make([]string, 0, len(report.Unhandled))
	for _, group := range report.Unhandled {
		known = append(known, fmt.Sprintf("%s (%d PRs, %s)", group.Signature, group.Count, strings.Join(group.Checks, ", ")))
	}
	if len(known) == 0 {
		return nil, fmt.Errorf("the report holds no unhandled failure")
	}
	return nil, fmt.Errorf("signature %q is not in the report; it holds:\n  %s", signature, strings.Join(known, "\n  "))
}

// ruleNameFor derives a rule name from the failing checks, which a person
// then replaces with one that says what the failure is.
func ruleNameFor(group *SweepUnhandled) string {
	name := "unnamed-pattern"
	if len(group.Checks) > 0 {
		name = nameRE.ReplaceAllString(strings.ToLower(group.Checks[0]), "-")
		name = strings.Trim(name, "-")
	}
	return name + "-" + group.Signature[:6]
}

var nameRE = regexp.MustCompile(`[^a-z0-9]+`)

// draftFile is one document of the draft, at the path it takes inside the
// catalogue and the path it is written to on disk. The pull request carries
// repoPath, which is where the catalogue lives whatever --path names.
type draftFile struct {
	repoPath string
	path     string
	body     string
}

// buildDraft renders the rule skeleton and its two scenarios. The action is
// left blank, so the draft fails validation until a person names one.
func buildDraft(name string, group *SweepUnhandled) []draftFile {
	rule := fmt.Sprintf(`name: %s
summary: TODO say in one line what this failure is.
source: TODO cite the runbook row or the PRs this came from.
match:
  states: [failed]
  check:
    name: %q
  log:
    source: actions
    pattern: 'TODO an expression that matches the excerpt below and nothing else'
action:
  # TODO name one action: update-branch, rerun-failed, circleci-retry, close,
  # mark-wait, dispatch-align-workflow, fix-protection-context, strict-chain.
  name: ""
evidence:
  reason: TODO what the PR should say once this ran.
`, name, firstCheck(group))

	matches := fmt.Sprintf(`name: TODO what this case shows
# Recorded from %s
subject:
  state: failed
  kind: renovate
  title: "TODO the PR title"
  failing: [%s]
  logs:
    "actions:%s": |
%s
expect:
  rule: %s
`, strings.Join(group.PRs, ", "), quoteList(group.Checks), firstCheck(group), indent(group.Excerpt, 6), name)

	refuses := fmt.Sprintf(`name: TODO a neighbouring failure this rule must leave alone
subject:
  state: failed
  kind: renovate
  title: "TODO the PR title"
  failing: [%s]
  logs:
    "actions:%s": |
      TODO a real excerpt of a different failure on the same check
expect:
  rule: ""
`, quoteList(group.Checks), firstCheck(group))

	scenarioDir := filepath.Join(rules.ScenarioDir, name)
	return []draftFile{
		{repoPath: path.Join(rules.DefaultDir, name+".yaml"), body: rule},
		{repoPath: path.Join(rules.DefaultDir, scenarioDir, "matches.yaml"), body: matches},
		{repoPath: path.Join(rules.DefaultDir, scenarioDir, "refuses.yaml"), body: refuses},
	}
}

// writeDraft writes the draft under --path and fills in each file's local
// path.
func writeDraft(name string, files []draftFile) error {
	if err := os.MkdirAll(filepath.Join(rulesFlags.path, rules.ScenarioDir, name), 0o750); err != nil {
		return err
	}
	for i, f := range files {
		local := filepath.Join(rulesFlags.path, strings.TrimPrefix(f.repoPath, rules.DefaultDir+"/"))
		if err := os.WriteFile(local, []byte(f.body), 0o600); err != nil {
			return err
		}
		files[i].path = local
	}
	return nil
}

func firstCheck(group *SweepUnhandled) string {
	if len(group.Checks) == 0 {
		return "TODO the failing check"
	}
	return group.Checks[0]
}

func quoteList(names []string) string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(out, ", ")
}

// indent lays an excerpt out as a YAML block scalar. The excerpt is
// stripped first: a raw escape from a colouring runner is not valid content
// in YAML, and a fixture that carries one cannot be read back.
func indent(body string, by int) string {
	pad := strings.Repeat(" ", by)
	lines := strings.Split(strings.TrimRight(logs.PlainText(body), "\n"), "\n")
	for i, line := range lines {
		lines[i] = pad + strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

// draftBranch is where a drafted rule lands. One branch per rule, so a
// second draft of the same pattern reports the open pull request rather than
// opening a second one.
func draftBranch(name string) string { return "rule/" + name }

// openDraftPR commits the draft and opens the pull request that carries it.
// marge has no git workspace and no push credential, so the tree, the
// commit, the branch and the pull request are all made through the API with
// the token the sweep already runs under. That token needs contents:write
// and pull-requests:write on the repository.
func openDraftPR(ctx context.Context, client *github.Client, owner, repo, base, name string, files []draftFile, group *SweepUnhandled) (*github.PullRequest, error) {
	baseRef, _, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+base)
	if err != nil {
		return nil, fmt.Errorf("reading %s/%s@%s: %w", owner, repo, base, err)
	}
	baseCommit, _, err := client.Git.GetCommit(ctx, owner, repo, baseRef.GetObject().GetSHA())
	if err != nil {
		return nil, fmt.Errorf("reading the head commit of %s: %w", base, err)
	}

	entries := make([]*github.TreeEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, &github.TreeEntry{
			Path:    new(f.repoPath),
			Mode:    new("100644"),
			Type:    new("blob"),
			Content: new(f.body),
		})
	}
	tree, _, err := client.Git.CreateTree(ctx, owner, repo, baseCommit.GetTree().GetSHA(), entries)
	if err != nil {
		return nil, fmt.Errorf("writing the draft tree: %w", err)
	}

	commit, _, err := client.Git.CreateCommit(ctx, owner, repo, github.Commit{
		Message: new(draftCommitMessage(name, group)),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: baseRef.GetObject().SHA}},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("committing the draft: %w", err)
	}

	branch := draftBranch(name)
	_, _, err = client.Git.CreateRef(ctx, owner, repo, github.CreateRef{
		Ref: "refs/heads/" + branch,
		SHA: commit.GetSHA(),
	})
	if err != nil {
		if open := openPRFor(ctx, client, owner, repo, branch); open != nil {
			return open, nil
		}
		return nil, fmt.Errorf("creating branch %s: %w", branch, err)
	}

	pull, _, err := client.PullRequests.Create(ctx, owner, repo, github.CreatePullRequest{
		Title: new(draftPRTitle(name)),
		Head:  branch,
		Base:  base,
		Body:  new(draftPRBody(name, group)),
		Draft: new(true),
	})
	if err != nil {
		return nil, fmt.Errorf("opening the pull request for %s: %w", branch, err)
	}
	return pull, nil
}

// openPRFor returns the pull request already standing on a branch, or nil.
// A branch that exists means the pattern was drafted before, and a second
// pull request for it would be noise.
func openPRFor(ctx context.Context, client *github.Client, owner, repo, branch string) *github.PullRequest {
	pulls, _, err := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:  owner + ":" + branch,
		State: "open",
	})
	if err != nil || len(pulls) == 0 {
		return nil
	}
	return pulls[0]
}

func draftPRTitle(name string) string {
	return "feat(rules): draft " + name
}

func draftCommitMessage(name string, group *SweepUnhandled) string {
	return fmt.Sprintf("%s\n\nDrafted from signature %s, seen on %d PR(s): %s.\nThe action is blank, so the catalogue does not carry this rule yet.\n",
		draftPRTitle(name), group.Signature, group.Count, strings.Join(group.PRs, ", "))
}

func draftPRBody(name string, group *SweepUnhandled) string {
	return fmt.Sprintf(`## Problem

A failure no rule recognises, signature %s, on %d PR(s): %s. The sweep reports it and applies nothing.

## Change

A rule skeleton for %s and the pair of scenarios its fixtures need, built from those PRs. `+
		"`action:`"+` is blank and every TODO is unanswered, so `+"`marge rules validate`"+` refuses this as it stands.

Name the action, write the log pattern against the recorded excerpt, and give the refusing scenario a real excerpt of a neighbouring failure. Push to `+"`%s`"+`.
`, group.Signature, group.Count, strings.Join(group.PRs, ", "), name, draftBranch(name))
}

var signaturesFlags struct {
	org   string
	days  int
	limit int
}

func init() {
	rulesSignaturesCmd.Flags().StringVar(&signaturesFlags.org, "org", rules.DefaultOwner, "Organization whose pull requests carry the markers")
	rulesSignaturesCmd.Flags().IntVar(&signaturesFlags.days, "days", 7, "How many days back to read; 0 reads everything the search returns")
	rulesSignaturesCmd.Flags().IntVar(&signaturesFlags.limit, "limit", 10, "How many signatures to print; 0 prints all of them")
	rulesCmd.AddCommand(rulesSignaturesCmd)
}

var rulesSignaturesCmd = &cobra.Command{
	Use:   "signatures",
	Short: "Count the failures no rule recognised, by signature",
	Long: `Read the markers the sweep left on the pull requests it could not handle and
count them by signature, the one on the most pull requests first.

A signature here is what "marge rules draft" takes. The count is what says
whether a pattern is worth a rule: one pull request is an accident, eleven
over five repositories is a rule waiting to be written.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := gh.NewClient(cmd.Context())
		if err != nil {
			return err
		}
		search := patterns.Search{Client: client, Org: signaturesFlags.org}
		if signaturesFlags.days > 0 {
			search.Since = time.Now().AddDate(0, 0, -signaturesFlags.days)
		}
		groups, err := search.Top(cmd.Context())
		if err != nil {
			return err
		}
		if len(groups) == 0 {
			fmt.Println("no unrecognised failure carries a marker in that window")
			return nil
		}
		for i, group := range groups {
			if signaturesFlags.limit > 0 && i == signaturesFlags.limit {
				fmt.Printf("and %d more\n", len(groups)-signaturesFlags.limit)
				break
			}
			fmt.Printf("%s  %3d PR(s)  %s\n", group.Signature, group.Count, strings.Join(group.Checks, ", "))
			fmt.Printf("            %s\n", strings.Join(group.PRs, " "))
		}
		return nil
	},
}
