package cmd

import (
	"fmt"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newSelfUpdateCmd())
}

func newSelfUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "self-update",
		Short: "Update marge to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkReleased(version); err != nil {
				return err
			}

			source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
			if err != nil {
				return fmt.Errorf("creating update source: %w", err)
			}

			updater, err := selfupdate.NewUpdater(selfupdate.Config{
				Source: source,
			})
			if err != nil {
				return fmt.Errorf("creating updater: %w", err)
			}

			latest, found, err := updater.DetectLatest(cmd.Context(), selfupdate.ParseSlug("teemow/marge"))
			if err != nil {
				return fmt.Errorf("detecting latest version: %w", err)
			}
			if !found {
				return fmt.Errorf("no release found")
			}

			if latest.LessOrEqual(version) {
				fmt.Printf("Already up to date (version %s)\n", version)
				return nil
			}

			fmt.Printf("Updating from %s to %s...\n", version, latest.Version())

			exe, err := selfupdate.ExecutablePath()
			if err != nil {
				return fmt.Errorf("finding executable path: %w", err)
			}

			if err := updater.UpdateTo(cmd.Context(), latest, exe); err != nil {
				return fmt.Errorf("updating binary: %w", err)
			}

			fmt.Printf("Successfully updated to %s\n", latest.Version())
			return nil
		},
	}
}

// checkReleased rejects a build version that cannot be compared with a
// release. Binaries built without ldflags carry "dev", and go-selfupdate
// panics when asked to compare anything that is not a semantic version.
func checkReleased(v string) error {
	if _, err := semver.NewVersion(v); err != nil {
		return fmt.Errorf("self-update is only available for released builds (current version: %s)", v)
	}
	return nil
}
