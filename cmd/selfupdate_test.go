package cmd

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
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

func TestReleaseIdentityPinsWorkflowIssuerAndRepository(t *testing.T) {
	identity := releaseIdentity(repository)
	// The fields a real release certificate carries (marge v0.8.0).
	ok := certificate.Summary{
		SubjectAlternativeName: releaseWorkflow,
		Extensions: certificate.Extensions{
			Issuer:              releaseIssuer,
			SourceRepositoryURI: "https://github.com/" + repository,
			SourceRepositoryRef: "refs/heads/main",
			RunnerEnvironment:   "github-hosted",
		},
	}
	if err := identity.Verify(ok); err != nil {
		t.Fatalf("a release built by the shared workflow must match: %v", err)
	}

	for name, change := range map[string]func(*certificate.Summary){
		"another repository": func(s *certificate.Summary) { s.SourceRepositoryURI = "https://github.com/teemow/patty" },
		"the repository's former home": func(s *certificate.Summary) {
			s.SourceRepositoryURI = "https://github.com/" + formerRepository
		},
		"a fork of the repository": func(s *certificate.Summary) { s.SourceRepositoryURI = "https://github.com/someone/marge" },
		"no source repository":     func(s *certificate.Summary) { s.SourceRepositoryURI = "" },
		"the repository's own CI": func(s *certificate.Summary) {
			s.SubjectAlternativeName = "https://github.com/" + repository + "/.github/workflows/ci.yml@refs/heads/main"
		},
		"the workflow from a branch": func(s *certificate.Summary) {
			s.SubjectAlternativeName = strings.Replace(releaseWorkflow, "refs/heads/main", "refs/heads/feature", 1)
		},
		"CircleCI as the issuer":  func(s *certificate.Summary) { s.Issuer = "https://oidc.circleci.com" },
		"a person as the subject": func(s *certificate.Summary) { s.SubjectAlternativeName = "someone@example.com" },
	} {
		s := ok
		change(&s)
		if err := identity.Verify(s); err == nil {
			t.Errorf("%s must not match", name)
		}
	}
}

// formerRepository is where marge lived, and where its releases up to v0.7.2
// were built, before it moved to giantswarm/marge.
const formerRepository = "teemow/marge"

// TestPublishedBundlesVerifyForTheirRepository checks bundles the release
// workflow published next to marge_linux_amd64 against a snapshot of the
// Sigstore public-good trust root, offline. The binaries stay out of the
// repository: their SHA-256, recorded in each bundle, is what the signature
// covers, so the check runs by digest.
//
// Each bundle verifies as a release of the repository it was built in and of
// no other. The v0.8.0 bundle was built here, so it verifies with the identity
// self-update ships. The v0.6.1 bundle was built at marge's former home, so it
// is refused for the current repository: a binary from before the move cannot
// self-update across it, and a release from the former home cannot be
// installed by a binary built here.
func TestPublishedBundlesVerifyForTheirRepository(t *testing.T) {
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

	for _, fixture := range []struct {
		file       string
		repository string
	}{
		{"testdata/marge-v0.8.0-linux-amd64.bundle", repository},
		{"testdata/marge-v0.6.1-linux-amd64.bundle", formerRepository},
	} {
		t.Run(filepath.Base(fixture.file), func(t *testing.T) {
			b, artifact := fixtureBundle(t, fixture.file)
			if _, err := verifier.Verify(b, verify.NewPolicy(artifact, verify.WithCertificateIdentity(releaseIdentity(fixture.repository)))); err != nil {
				t.Fatalf("the published bundle must verify as a release of %s: %v", fixture.repository, err)
			}
			// The same bundle, checked as a release of any other repository the
			// same workflow signs for: refused.
			for _, other := range []string{repository, formerRepository, "teemow/patty"} {
				if other == fixture.repository {
					continue
				}
				if _, err := verifier.Verify(b, verify.NewPolicy(artifact, verify.WithCertificateIdentity(releaseIdentity(other)))); err == nil {
					t.Errorf("a bundle for %s must not verify as a release of %s", fixture.repository, other)
				}
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
