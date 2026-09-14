package cmd

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

func TestCheckReleased(t *testing.T) {
	for _, v := range []string{"0.5.0", "v0.5.0", "1.2.3-rc.1"} {
		if err := checkReleased(v); err != nil {
			t.Errorf("checkReleased(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"dev", "", "main", "abc123"} {
		err := checkReleased(v)
		if err == nil {
			t.Errorf("checkReleased(%q) = nil, want error", v)
			continue
		}
		if want := "self-update is only available for released builds (current version: " + v + ")"; err.Error() != want {
			t.Errorf("checkReleased(%q) = %q, want %q", v, err, want)
		}
	}
}

// TestSelfUpdateDevBuild runs the command as a `go build` without ldflags
// produces it. It must fail with a clear message instead of panicking, and it
// must do so before touching the network.
func TestSelfUpdateDevBuild(t *testing.T) {
	prev := version
	t.Cleanup(func() { SetVersion(prev) })
	SetVersion("dev")

	cmd := newSelfUpdateCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("self-update with version dev succeeded, want error")
	}
	if !strings.Contains(err.Error(), "only available for released builds") || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestReleaseIdentityIsTheRepositorysCircleCIPipeline pins what self-update
// accepts: a certificate issued by CircleCI to a pipeline of this repository.
// The shared validator owns the exact matchers; this test guards the slug
// they are built from, so a rename of the repository cannot silently keep the
// old identity.
func TestReleaseIdentityIsTheRepositorysCircleCIPipeline(t *testing.T) {
	identity := selfupdatecosign.Identity(repository)
	if got, want := identity.SourceRepositoryURI, selfupdatecosign.SourceRepositoryURI(repository); got != want {
		t.Fatalf("SourceRepositoryURI = %q, want %q", got, want)
	}
	if !strings.HasSuffix(identity.SourceRepositoryURI, "/"+repository) {
		t.Fatalf("the identity must name %s, got %q", repository, identity.SourceRepositoryURI)
	}
}

// TestPublishedBundlesVerifyForTheCircleCIPipelineOnly checks bundles the
// release pipelines published next to marge-linux-amd64 against a snapshot of
// the Sigstore public-good trust root, offline. The binaries stay out of the
// repository: their SHA-256, recorded in each bundle, is what the signature
// covers, so the check runs by digest.
//
// The v0.9.0 bundle is the first one this repository's CircleCI pipeline
// signed: it verifies with the identity self-update ships. The two bundles
// the former GitHub Actions release workflow published (v0.6.1 at the
// repository's former home, v0.8.0 here) are genuine Sigstore bundles but are
// refused: a binary built here never installs a release of the Actions era,
// whichever repository signed it.
func TestPublishedBundlesVerifyForTheCircleCIPipelineOnly(t *testing.T) {
	material, err := root.NewTrustedRootFromJSON(read(t, "testdata/trusted_root.json"))
	if err != nil {
		t.Fatalf("loading the trust root snapshot: %v", err)
	}
	verifier, err := verify.NewVerifier(material,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
		verify.WithSignedCertificateTimestamps(1),
	)
	if err != nil {
		t.Fatalf("preparing the verifier: %v", err)
	}
	identity := verify.WithCertificateIdentity(selfupdatecosign.Identity(repository))

	for _, fixture := range []struct {
		file     string
		accepted bool
	}{
		{"testdata/marge-v0.9.0-linux-amd64.bundle", true},
		{"testdata/marge-v0.8.0-linux-amd64.bundle", false},
		{"testdata/marge-v0.6.1-linux-amd64.bundle", false},
	} {
		t.Run(filepath.Base(fixture.file), func(t *testing.T) {
			b, artifact := fixtureBundle(t, fixture.file)
			// The bundle is genuine: it verifies without an identity pin.
			if _, err := verifier.Verify(b, verify.NewPolicy(artifact, verify.WithoutIdentitiesUnsafe())); err != nil {
				t.Fatalf("the fixture must be a valid Sigstore bundle: %v", err)
			}
			_, err := verifier.Verify(b, verify.NewPolicy(artifact, identity))
			switch {
			case fixture.accepted && err != nil:
				t.Fatalf("the bundle signed by this repository's CircleCI pipeline must verify: %v", err)
			case !fixture.accepted && err == nil:
				t.Fatal("a bundle signed by GitHub Actions must not verify as a CircleCI build of " + repository)
			}
		})
	}
}

// fixtureBundle parses a published bundle and returns it with the artifact
// policy for the binary it signs, taken from the digest the bundle records.
func fixtureBundle(t *testing.T, name string) (*bundle.Bundle, verify.ArtifactPolicyOption) {
	t.Helper()
	raw := read(t, name)
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		t.Fatalf("parsing the bundle: %v", err)
	}
	var recorded struct {
		MessageSignature struct {
			MessageDigest struct {
				Digest string `json:"digest"`
			} `json:"messageDigest"`
		} `json:"messageSignature"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("reading the digest from the bundle: %v", err)
	}
	digest, err := base64.StdEncoding.DecodeString(recorded.MessageSignature.MessageDigest.Digest)
	if err != nil || len(digest) != 32 {
		t.Fatalf("the bundle should record the binary's SHA-256: %v", err)
	}
	return &b, verify.WithArtifactDigest("sha256", digest)
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return data
}
