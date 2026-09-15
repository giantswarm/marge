package pr

import (
	"testing"

	"github.com/google/go-github/v92/github"
)

// The patch that motivated the rule: giantswarm/agentgateway#29, where
// Renovate moved the version comment of an action whose pinned commit did
// not change.
const commentOnlyPatch = `@@ -12,7 +12,7 @@ jobs:
     steps:
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0
       - name: Build
         run: make build`

func workflowFile(name, patch string) *github.CommitFile {
	return &github.CommitFile{Filename: new(name), Patch: new(patch)}
}

func TestNoOpDiff(t *testing.T) {
	tests := []struct {
		name  string
		files []*github.CommitFile
		want  bool
	}{
		{
			name:  "version comment only on an unchanged pinned SHA",
			files: []*github.CommitFile{workflowFile(".github/workflows/ci.yaml", commentOnlyPatch)},
			want:  true,
		},
		{
			name: "composite action definition counts too",
			files: []*github.CommitFile{workflowFile(".github/actions/setup/action.yml", `@@ -3,1 +3,1 @@
-    - uses: actions/setup-go@0aaccfd150d50ccaeb58ebd88d36e91967a5f35b # v5.0.1
+    - uses: actions/setup-go@0aaccfd150d50ccaeb58ebd88d36e91967a5f35b # v5.1.0`)},
			want: true,
		},
		{
			name: "several files, all comment-only",
			files: []*github.CommitFile{
				workflowFile(".github/workflows/ci.yaml", commentOnlyPatch),
				workflowFile(".github/workflows/release.yml", commentOnlyPatch),
			},
			want: true,
		},
		{
			name: "the pinned SHA moved",
			files: []*github.CommitFile{workflowFile(".github/workflows/ci.yaml", `@@ -12,1 +12,1 @@
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/checkout@08c6903cd8c0fde910a37f88322edcfb5dd907a8 # v7.0.1`)},
			want: false,
		},
		{
			name: "a tag ref carries the version itself",
			files: []*github.CommitFile{workflowFile(".github/workflows/ci.yaml", `@@ -12,1 +12,1 @@
-      - uses: actions/checkout@v5
+      - uses: actions/checkout@v7`)},
			want: false,
		},
		{
			name: "the line was re-indented",
			files: []*github.CommitFile{workflowFile(".github/workflows/ci.yaml", `@@ -12,1 +12,1 @@
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+    - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0`)},
			want: false,
		},
		{
			name: "a comment change outside a workflow is a real change",
			files: []*github.CommitFile{workflowFile("Makefile", `@@ -1,1 +1,1 @@
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0`)},
			want: false,
		},
		{
			name: "a real change alongside a comment change",
			files: []*github.CommitFile{
				workflowFile(".github/workflows/ci.yaml", commentOnlyPatch),
				workflowFile(".github/workflows/release.yml", `@@ -4,1 +4,1 @@
-      - run: make test
+      - run: make test-all`)},
			want: false,
		},
		{
			name: "an added line without a removed one",
			files: []*github.CommitFile{workflowFile(".github/workflows/ci.yaml", `@@ -12,1 +12,2 @@
       - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/setup-go@0aaccfd150d50ccaeb58ebd88d36e91967a5f35b # v5.1.0`)},
			want: false,
		},
		{
			// A GitHub patch carries no file header, so a "---" line in it
			// is a YAML document marker the PR deleted.
			name: "a deleted document marker is a real change",
			files: []*github.CommitFile{workflowFile(".github/workflows/ci.yaml", `@@ -1,5 +1,4 @@
----
 name: ci
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0`)},
			want: false,
		},
		{
			name:  "no files at all",
			files: nil,
			want:  false,
		},
		{
			name:  "a file whose patch GitHub did not deliver",
			files: []*github.CommitFile{{Filename: new(".github/workflows/ci.yaml")}},
			want:  false,
		},
		{
			name: "a file with no changed lines",
			files: []*github.CommitFile{workflowFile(".github/workflows/ci.yaml", `@@ -12,2 +12,2 @@
       - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
       - run: make build`)},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NoOpDiff(tt.files); got != tt.want {
				t.Errorf("NoOpDiff = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripTrailingComment(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"      - uses: foo/bar@abc # v5", "      - uses: foo/bar@abc"},
		{"      - uses: foo/bar@abc", "      - uses: foo/bar@abc"},
		{"# whole line", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripTrailingComment(tt.in); got != tt.want {
			t.Errorf("stripTrailingComment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
