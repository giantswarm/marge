package pr

import (
	"testing"

	"github.com/google/go-github/v91/github"
)

func commitFile(name, status string, changes int, patch string) *github.CommitFile {
	f := &github.CommitFile{
		SHA:      new("blob-" + name),
		Filename: new(name),
		Status:   new(status),
		Changes:  new(changes),
	}
	if patch != "" {
		f.Patch = new(patch)
	}
	return f
}

// The package.json hunk of a Renovate "typescript to v7" bump as GitHub
// returns it before and after a rebase onto a base that changed the line
// above it: the hunk header and the context line move, the changed lines
// do not.
const (
	pkgPatchBefore = "@@ -20,7 +20,7 @@\n   \"devDependencies\": {\n     \"prettier\": \"^3.6.2\",\n-    \"typescript\": \"^5.9.2\",\n+    \"typescript\": \"^7.0.0\",\n     \"vite\": \"^7.1.0\"\n   }\n }"
	pkgPatchAfter  = "@@ -24,7 +24,7 @@\n   \"devDependencies\": {\n     \"prettier\": \"^3.7.0\",\n-    \"typescript\": \"^5.9.2\",\n+    \"typescript\": \"^7.0.0\",\n     \"vite\": \"^7.1.0\"\n   }\n }"
	pkgPatchBumped = "@@ -24,7 +24,7 @@\n   \"devDependencies\": {\n     \"prettier\": \"^3.7.0\",\n-    \"typescript\": \"^5.9.2\",\n+    \"typescript\": \"^7.1.0\",\n     \"vite\": \"^7.1.0\"\n   }\n }"
	lockPatch      = "@@ -1201,9 +1201,9 @@\n     \"node_modules/typescript\": {\n-      \"version\": \"5.9.2\",\n-      \"resolved\": \"https://registry.npmjs.org/typescript/-/typescript-5.9.2.tgz\",\n+      \"version\": \"7.0.0\",\n+      \"resolved\": \"https://registry.npmjs.org/typescript/-/typescript-7.0.0.tgz\",\n       \"dev\": true,"
	lockPatchMoved = "@@ -1288,9 +1288,9 @@\n     \"node_modules/typescript\": {\n-      \"version\": \"5.9.2\",\n-      \"resolved\": \"https://registry.npmjs.org/typescript/-/typescript-5.9.2.tgz\",\n+      \"version\": \"7.0.0\",\n+      \"resolved\": \"https://registry.npmjs.org/typescript/-/typescript-7.0.0.tgz\",\n       \"dev\": true,\n       \"license\": \"Apache-2.0\","
)

func TestPatchID_stableAcrossRebase(t *testing.T) {
	before := []*github.CommitFile{
		commitFile("package.json", "modified", 2, pkgPatchBefore),
		commitFile("package-lock.json", "modified", 4, lockPatch),
	}
	// After the rebase GitHub also happens to list the files in another order.
	after := []*github.CommitFile{
		commitFile("package-lock.json", "modified", 4, lockPatchMoved),
		commitFile("package.json", "modified", 2, pkgPatchAfter),
	}

	got, want := PatchID(after, 2), PatchID(before, 2)
	if got == "" || got != want {
		t.Errorf("PatchID after rebase = %q, before = %q; want equal and non-empty", got, want)
	}
	if len(got) != patchIDLen {
		t.Errorf("PatchID length = %d, want %d", len(got), patchIDLen)
	}
}

func TestPatchID_changedLinesDiffer(t *testing.T) {
	v7 := []*github.CommitFile{commitFile("package.json", "modified", 2, pkgPatchAfter)}
	v71 := []*github.CommitFile{commitFile("package.json", "modified", 2, pkgPatchBumped)}

	if PatchID(v7, 1) == PatchID(v71, 1) {
		t.Error("a changed + line must change the patch ID")
	}
}

func TestPatchID_pathAndStatusMatter(t *testing.T) {
	base := commitFile("src/a.go", "modified", 2, "@@ -1,2 +1,2 @@\n-old\n+new")
	renamed := commitFile("src/a.go", "renamed", 2, "@@ -1,2 +1,2 @@\n-old\n+new")
	renamed.PreviousFilename = new("src/b.go")
	elsewhere := commitFile("src/c.go", "modified", 2, "@@ -1,2 +1,2 @@\n-old\n+new")

	one := PatchID([]*github.CommitFile{base}, 1)
	if one == PatchID([]*github.CommitFile{renamed}, 1) {
		t.Error("status and previous path must be part of the fingerprint")
	}
	if one == PatchID([]*github.CommitFile{elsewhere}, 1) {
		t.Error("the file path must be part of the fingerprint")
	}
}

func TestPatchID_binaryUsesBlobSHA(t *testing.T) {
	logo := commitFile("logo.png", "modified", 0, "")
	logo.SHA = new("aaaa")
	sameLogo := commitFile("logo.png", "modified", 0, "")
	sameLogo.SHA = new("aaaa")
	otherLogo := commitFile("logo.png", "modified", 0, "")
	otherLogo.SHA = new("bbbb")

	same := PatchID([]*github.CommitFile{logo}, 1)
	if same == "" || same != PatchID([]*github.CommitFile{sameLogo}, 1) {
		t.Error("a binary file with the same blob must fingerprint the same")
	}
	if same == PatchID([]*github.CommitFile{otherLogo}, 1) {
		t.Error("a binary file with another blob must fingerprint differently")
	}
}

func TestPatchID_unavailable(t *testing.T) {
	complete := []*github.CommitFile{commitFile("package.json", "modified", 2, pkgPatchAfter)}
	truncatedList := complete // one file returned for a two-file PR
	patchOmitted := []*github.CommitFile{
		commitFile("package.json", "modified", 2, pkgPatchAfter),
		commitFile("package-lock.json", "modified", 12000, ""), // diff too large, no patch
	}

	tests := []struct {
		name         string
		files        []*github.CommitFile
		changedFiles int
	}{
		{"no files", nil, 0},
		{"fewer files than the PR changed", truncatedList, 2},
		{"patch omitted for a changed file", patchOmitted, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PatchID(tt.files, tt.changedFiles); got != "" {
				t.Errorf("PatchID = %q, want \"\" (incomplete diff must not be fingerprinted)", got)
			}
		})
	}
	if PatchID(complete, 1) == "" || PatchID(complete, 0) == "" {
		t.Error("a complete diff (or one with an unknown file count) must fingerprint")
	}
}

func TestChangeID(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"chore(deps): update dependency typescript to v7", "typescript@v7"},
		{"Update dependency typescript to v7.1.0", "typescript@v7.1.0"},
		{"Update module github.com/spf13/cobra to v1.10.2", "github.com/spf13/cobra@v1.10.2"},
		{"fix(deps): update eslint monorepo to v10 (major)", "eslint@v10"},
		{"Bump lodash from 4.17.20 to 4.17.21", "lodash@4.17.21"},
		{"build(deps): bump go.opentelemetry.io/otel/sdk from 1.39.0 to 1.40.0 in the go_modules group across 1 directory", "go.opentelemetry.io/otel/sdk@1.40.0"},
		{"fix(deps): update all non-major dependencies", ""},
		{"Lock file maintenance", ""},
		{"Update npm (minor)", ""},
		{"fix(deps): update opentelemetry-go monorepo", ""},
		{"Refactor the build", ""},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := ChangeID(tt.title); got != tt.want {
				t.Errorf("ChangeID(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestFingerprint_Matches(t *testing.T) {
	tests := []struct {
		name string
		a, b Fingerprint
		want bool
	}{
		{"same patch id", Fingerprint{PatchID: "p1"}, Fingerprint{PatchID: "p1"}, true},
		{"patch id decides even when change ids agree", Fingerprint{PatchID: "p1", ChangeID: "ts@v7"}, Fingerprint{PatchID: "p2", ChangeID: "ts@v7"}, false},
		{"patch id decides even when change ids differ", Fingerprint{PatchID: "p1", ChangeID: "ts@v7"}, Fingerprint{PatchID: "p1", ChangeID: "ts@v8"}, true},
		{"change id fallback, same", Fingerprint{PatchID: "p1", ChangeID: "ts@v7"}, Fingerprint{ChangeID: "ts@v7"}, true},
		{"change id fallback, version bump", Fingerprint{ChangeID: "ts@v6"}, Fingerprint{PatchID: "p1", ChangeID: "ts@v7"}, false},
		{"nothing comparable", Fingerprint{PatchID: "p1"}, Fingerprint{ChangeID: "ts@v7"}, false},
		{"both empty", Fingerprint{}, Fingerprint{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Matches(tt.b); got != tt.want {
				t.Errorf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
	if (Fingerprint{}).Comparable() || !(Fingerprint{ChangeID: "x@v1"}).Comparable() {
		t.Error("Comparable must be false only for an empty fingerprint")
	}
}
