package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
)

var rulesFlags struct {
	path string
	repo string
	ref  string
}

func init() {
	for _, cmd := range []*cobra.Command{rulesValidateCmd, rulesTestCmd} {
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
		fmt.Printf("%d rules, catalogue %s\n", len(catalogue.Rules), catalogue.Digest)
		for _, rule := range catalogue.Rules {
			fmt.Printf("  %-34s %s\n", rule.Name, rule.Action.Name)
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

// loadCatalogueForCLI reads the catalogue from disk, or from a repository
// when --repo asks for it.
func loadCatalogueForCLI(ctx context.Context) (*rules.Catalogue, error) {
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
