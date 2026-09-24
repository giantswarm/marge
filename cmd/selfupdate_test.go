package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/creativeprojects/go-selfupdate"
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

// fakeSource stands in for GitHub: one release, and the bytes every asset
// download returns.
type fakeSource struct {
	release fakeRelease
	assets  map[int64][]byte
}

func (s *fakeSource) ListReleases(context.Context, selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	return []selfupdate.SourceRelease{s.release}, nil
}

func (s *fakeSource) DownloadReleaseAsset(_ context.Context, _ *selfupdate.Release, id int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.assets[id])), nil
}

type fakeAsset struct {
	id   int64
	name string
}

func (a fakeAsset) GetID() int64                  { return a.id }
func (a fakeAsset) GetName() string               { return a.name }
func (a fakeAsset) GetSize() int                  { return 3 }
func (a fakeAsset) GetBrowserDownloadURL() string { return "https://example.test/" + a.name }

type fakeRelease struct {
	tag    string
	assets []selfupdate.SourceAsset
}

func (r fakeRelease) GetID() int64              { return 1 }
func (r fakeRelease) GetTagName() string        { return r.tag }
func (r fakeRelease) GetDraft() bool            { return false }
func (r fakeRelease) GetPrerelease() bool       { return false }
func (r fakeRelease) GetPublishedAt() time.Time { return time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC) }
func (r fakeRelease) GetReleaseNotes() string   { return "notes" }
func (r fakeRelease) GetName() string           { return r.tag }
func (r fakeRelease) GetURL() string {
	return "https://github.com/" + repository + "/releases/tag/" + r.tag
}
func (r fakeRelease) GetAssets() []selfupdate.SourceAsset { return r.assets }

// accepting stands in for the cosign validator when a download verifies.
type accepting struct{}

func (accepting) GetValidationAssetName(name string) string { return name + ".bundle" }
func (accepting) Validate(string, []byte, []byte) error     { return nil }

// A verified release replaces the file the executable path names, symbolic
// links resolved, with a single rename: the file keeps its mode, the link
// keeps naming it, and nothing is left beside it.
func TestSelfUpdateInstallsAVerifiedReleaseInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows the update is go-selfupdate's own swap")
	}
	asset := "marge-" + runtime.GOOS + "-" + runtime.GOARCH
	src := &fakeSource{
		release: fakeRelease{tag: "v99.0.0", assets: []selfupdate.SourceAsset{
			fakeAsset{1, asset},
			fakeAsset{2, asset + ".bundle"},
		}},
		assets: map[int64][]byte{1: []byte("a newer marge"), 2: []byte("its bundle")},
	}
	exe := filepath.Join(t.TempDir(), "marge")
	if err := os.WriteFile(exe, []byte("the marge that is installed right now"), 0o755); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	// A mode the update would not pick itself, set whatever the umask.
	if err := os.Chmod(exe, 0o750); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "marge")
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	prevSource, prevExe, prevValidator, prevVersion := selfUpdateSource, selfUpdateExecutable, selfUpdateValidator, version
	t.Cleanup(func() {
		selfUpdateSource, selfUpdateExecutable, selfUpdateValidator = prevSource, prevExe, prevValidator
		SetVersion(prevVersion)
	})
	selfUpdateSource = src
	selfUpdateExecutable = func() (string, error) { return link, nil }
	selfUpdateValidator = func() selfupdate.Validator { return accepting{} }
	SetVersion("0.1.0")

	cmd := newSelfUpdateCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("self-update: %v", err)
	}

	got, err := os.ReadFile(exe) //nolint:gosec // the test's own temp file
	if err != nil || string(got) != "a newer marge" {
		t.Errorf("%s holds %q (%v), want the release's binary", exe, got, err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o750 {
		t.Errorf("%s has mode %v, want it to keep -rwxr-x---", exe, mode)
	}
	if dest, err := os.Readlink(link); err != nil || dest != exe {
		t.Errorf("the link names %q (%v), want %s", dest, err, exe)
	}
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %q, want only marge", filepath.Dir(exe), names)
	}
}
