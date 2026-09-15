package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/logs"
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
	from   string
	name   string
	dryRun bool
}

func init() {
	rulesDraftCmd.Flags().StringVar(&draftFlags.from, "from", "-", "Sweep report to read the signature from; - reads standard input")
	rulesDraftCmd.Flags().StringVar(&draftFlags.name, "name", "", "Name of the rule to draft (default: derived from the failing checks)")
	rulesDraftCmd.Flags().BoolVar(&draftFlags.dryRun, "dry-run", false, "Write the files only; print no commands to open a pull request")
	rulesCmd.AddCommand(rulesDraftCmd)
}

var rulesDraftCmd = &cobra.Command{
	Use:   "draft <signature>",
	Short: "Draft a rule and its scenarios from an unrecognised failure",
	Long: `Read one unhandled signature out of a sweep report (marge sweep --output json)
and write a rule skeleton with a pair of scenarios built from the PRs that
carry it, then print the commands that open the draft pull request.

The skeleton leaves the action blank on purpose: it does not validate until a
person names one, so promoting a pattern is editing a draft rather than
writing one from nothing.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		report, err := readSweepReport(draftFlags.from)
		if err != nil {
			return err
		}
		group, err := findSignature(report, args[0])
		if err != nil {
			return err
		}

		name := draftFlags.name
		if name == "" {
			name = ruleNameFor(group)
		}
		files, err := writeDraft(name, group)
		if err != nil {
			return err
		}
		for _, path := range files {
			fmt.Println("wrote", path)
		}
		fmt.Printf("\nName the action in %s, then run: marge rules validate && marge rules test\n", files[0])
		if draftFlags.dryRun {
			return nil
		}
		fmt.Println("\nOpen the draft pull request with:")
		fmt.Printf("  git checkout -b rule/%s && git add %s && git commit && gh pr create --draft\n", name, rulesFlags.path)
		return nil
	},
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

// writeDraft writes the rule skeleton and its two scenarios. The action is
// left blank, so the draft fails validation until a person names one.
func writeDraft(name string, group *SweepUnhandled) ([]string, error) {
	rulePath := filepath.Join(rulesFlags.path, name+".yaml")
	scenarioDir := filepath.Join(rulesFlags.path, rules.ScenarioDir, name)
	if err := os.MkdirAll(scenarioDir, 0o755); err != nil {
		return nil, err
	}

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

	files := []struct{ path, body string }{
		{rulePath, rule},
		{filepath.Join(scenarioDir, "matches.yaml"), matches},
		{filepath.Join(scenarioDir, "refuses.yaml"), refuses},
	}
	written := make([]string, 0, len(files))
	for _, f := range files {
		if err := os.WriteFile(f.path, []byte(f.body), 0o600); err != nil {
			return nil, err
		}
		written = append(written, f.path)
	}
	return written, nil
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
