package cmd

import (
	"errors"
	"fmt"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
	"github.com/spf13/cobra"
)

// repository is the GitHub repository marge releases are published from.
const repository = "giantswarm/marge"

func init() {
	rootCmd.AddCommand(newSelfUpdateCmd())
}

func newSelfUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "self-update",
		Short: "Update marge to the latest signed release",
		Long: `Downloads the latest release of marge and replaces this binary with it.

Release binaries are signed in CI (cosign, keyless) and published next to
their Sigstore bundle. The download is installed only after that bundle
verifies for a CircleCI build of ` + repository + `; a release without a
bundle, or a download that does not match its signature, is refused and the
installed binary is left untouched.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkReleased(version); err != nil {
				return err
			}

			source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
			if err != nil {
				return fmt.Errorf("creating update source: %w", err)
			}

			// The validator makes DetectLatest look for <asset>.bundle next to
			// the binary and UpdateTo verify the download against it before
			// anything is written.
			updater, err := selfupdate.NewUpdater(selfupdate.Config{
				Source:    source,
				Validator: selfupdatecosign.New(repository),
			})
			if err != nil {
				return fmt.Errorf("creating updater: %w", err)
			}

			latest, found, err := updater.DetectLatest(cmd.Context(), selfupdate.ParseSlug(repository))
			if errors.Is(err, selfupdate.ErrValidationAssetNotFound) {
				return fmt.Errorf("the latest release of %s has no signature bundle; refusing to install an unverified binary", repository)
			}
			if err != nil {
				return fmt.Errorf("detecting latest version: %w", err)
			}
			if !found {
				return errors.New("no release found")
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
				return fmt.Errorf("updating binary (the installed %s is unchanged): %w", version, err)
			}

			fmt.Printf("Successfully updated to %s (signature verified)\n", latest.Version())
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
