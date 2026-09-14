package cmd

import (
	"errors"
	"fmt"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/spf13/cobra"
)

// repository is the GitHub repository marge releases are published from.
const repository = "giantswarm/marge"

// Releases are signed keyless with cosign by the shared release workflow.
// These are the identity fields Fulcio records in the signing certificate.
const (
	releaseWorkflow = "https://github.com/teemow/github-workflows/.github/workflows/release-go.yml@refs/heads/main"
	releaseIssuer   = "https://token.actions.githubusercontent.com"
)

func init() {
	rootCmd.AddCommand(newSelfUpdateCmd())
}

func newSelfUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "self-update",
		Short: "Update marge to the latest signed release",
		Long: `Downloads the latest release of marge and replaces this binary with it.

The download is installed only after its cosign signature bundle verifies as
a build of ` + repository + ` by the shared release workflow; otherwise the
installed binary is left untouched.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkReleased(version); err != nil {
				return err
			}

			source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
			if err != nil {
				return fmt.Errorf("creating update source: %w", err)
			}

			updater, err := selfupdate.NewUpdater(selfupdate.Config{
				Source:    source,
				Validator: selfupdatecosign.New(repository, selfupdatecosign.WithIdentity(releaseIdentity(repository))),
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

// releaseIdentity is what the certificate in a release's Sigstore bundle must
// say: issued by GitHub Actions to the shared release workflow, for a build of
// the given repository. Pinning the repository is what keeps another
// repository's release, signed by the same workflow, from being installed.
func releaseIdentity(repository string) verify.CertificateIdentity {
	return verify.CertificateIdentity{
		SubjectAlternativeName: verify.SubjectAlternativeNameMatcher{SubjectAlternativeName: releaseWorkflow},
		Issuer:                 verify.IssuerMatcher{Issuer: releaseIssuer},
		Extensions:             certificate.Extensions{SourceRepositoryURI: "https://github.com/" + repository},
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
