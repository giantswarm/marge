// Package project carries the build identifiers of the marge binary.
package project

import "runtime/debug"

// dev is the default for unset build identifiers. Local `go build`
// invocations without ldflags keep this so `marge version` stays printable
// and `marge self-update` refuses the build with a clear message.
const dev = "dev"

// devel is the module version runtime/debug reports for a build that carries
// no resolvable VCS tag (no .git, or built outside a module checkout).
const devel = "(devel)"

// Build identifiers. The generated Makefile and the architect-orb `go-build`
// job set them at link time via `-X` ldflags: `version` from gitsemver,
// `gitSHA` from the commit and `buildTimestamp` from the UTC build time. A
// build without ldflags derives its version from the Go build info instead
// (see Version), which the toolchain stamps from the VCS tag.
var (
	version        = dev
	gitSHA         = dev
	buildTimestamp = "unknown"
)

// Version returns the best human-readable build identifier available, in
// order: an explicitly injected `version` ldflag, the VCS version stamped into
// the Go build info, the injected commit SHA, and finally the placeholder
// "dev".
func Version() string {
	if version != dev && version != "" {
		return version
	}
	if v := buildInfoVersion(); v != "" {
		return v
	}
	if gitSHA != dev {
		return gitSHA
	}
	return dev
}

// buildInfoVersion reads the main module version the Go toolchain embedded from
// version control. It returns "" when no usable version is present -- either no
// build info, or the "(devel)" placeholder a tag-less build produces -- so
// Version can fall through to the next source.
var buildInfoVersion = func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if v := info.Main.Version; v != "" && v != devel {
		return v
	}
	return ""
}

// GitSHA returns the commit SHA the binary was built from.
func GitSHA() string { return gitSHA }

// BuildTimestamp returns the UTC build time in RFC 3339 format, or
// "unknown" when no ldflag was injected.
func BuildTimestamp() string { return buildTimestamp }
