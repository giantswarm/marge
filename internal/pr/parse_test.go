package pr

import (
	"testing"
)

func TestParsePRURL(t *testing.T) {
	owner, repo, number, err := ParsePRURL("https://github.com/giantswarm/happa/pull/4808")
	if err != nil {
		t.Fatalf("ParsePRURL error: %v", err)
	}
	if owner != "giantswarm" || repo != "happa" || number != 4808 {
		t.Errorf("got %s/%s#%d, want giantswarm/happa#4808", owner, repo, number)
	}

	for _, bad := range []string{
		"https://github.com/giantswarm/happa",
		"https://github.com/giantswarm/happa/issues/42",
		"https://github.com/giantswarm/happa/pull/abc",
	} {
		if _, _, _, err := ParsePRURL(bad); err == nil {
			t.Errorf("ParsePRURL(%q) expected error, got nil", bad)
		}
	}
}

func TestExtractOwnerRepo(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{
			name:      "standard PR URL",
			url:       "https://github.com/giantswarm/flux-app/pull/42",
			wantOwner: "giantswarm",
			wantRepo:  "flux-app",
		},
		{
			name:      "repo URL with trailing path",
			url:       "https://github.com/giantswarm/flux-app/issues/1",
			wantOwner: "giantswarm",
			wantRepo:  "flux-app",
		},
		{
			name:    "empty string",
			url:     "",
			wantErr: true,
		},
		{
			name:    "only owner",
			url:     "https://github.com/giantswarm",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := ExtractOwnerRepo(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got owner=%q repo=%q", owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}

func TestExtractDependencyName(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		// Renovate: "Update dependency X to Y"
		{
			name:  "renovate update dependency",
			title: "Update dependency github.com/foo/bar to v1.2.3",
			want:  "github.com/foo/bar",
		},
		{
			name:  "renovate update dependency case insensitive",
			title: "update dependency lodash to 4.17.21",
			want:  "lodash",
		},
		// Renovate: "Update X to Y" (without "dependency")
		{
			name:  "renovate update without dependency keyword",
			title: "Update golang.org/x/net to v0.5.0",
			want:  "golang.org/x/net",
		},
		// Renovate: "Update module X to Y"
		{
			name:  "renovate update module",
			title: "Update module github.com/aws/aws-sdk-go to v1.44.0",
			want:  "github.com/aws/aws-sdk-go",
		},
		// Renovate: "Update github-actions action X to Y"
		{
			name:  "renovate update github-actions action",
			title: "Update github-actions action actions/checkout to v4",
			want:  "actions/checkout",
		},
		// Renovate: "Pin dependency X to Y"
		{
			name:  "renovate pin dependency",
			title: "Pin dependency typescript to 5.3.3",
			want:  "typescript",
		},
		// Renovate: "Replace dependency X with Y"
		{
			name:  "renovate replace dependency",
			title: "Replace dependency io/ioutil with os",
			want:  "io/ioutil",
		},
		// Renovate: "Lock file maintenance"
		{
			name:  "renovate lock file maintenance",
			title: "Lock file maintenance",
			want:  "Lock file maintenance",
		},
		// Renovate: rust crate
		{
			name:  "renovate rust crate",
			title: "fix(deps): update rust crate kube to v3",
			want:  "kube",
		},
		{
			name:  "renovate rust crate whoami",
			title: "fix(deps): update rust crate whoami to v2",
			want:  "whoami",
		},
		// Renovate: terraform provider
		{
			name:  "renovate terraform aws",
			title: "chore(deps): update terraform aws to v6",
			want:  "aws",
		},
		{
			name:  "renovate terraform ignition",
			title: "chore(deps): update terraform ignition to v2",
			want:  "ignition",
		},
		{
			name:  "renovate terraform cloudflare",
			title: "chore(deps): update terraform cloudflare to v5",
			want:  "cloudflare",
		},
		// Renovate: monorepo
		{
			name:  "renovate monorepo without version",
			title: "fix(deps): update opentelemetry-go monorepo",
			want:  "opentelemetry-go",
		},
		{
			name:  "renovate monorepo with version",
			title: "chore(deps): update eslint monorepo to v10 (major)",
			want:  "eslint",
		},
		// Renovate: group update
		{
			name:  "renovate all non-major dependencies",
			title: "fix(deps): update all non-major dependencies",
			want:  "all non-major dependencies",
		},
		// Conventional commit prefix: "fix(deps): update module X"
		{
			name:  "conventional commit update module",
			title: "fix(deps): update module github.com/mark3labs/mcp-go to v0.44.1",
			want:  "github.com/mark3labs/mcp-go",
		},
		{
			name:  "conventional commit update module go-github",
			title: "fix(deps): update module github.com/google/go-github/v60 to v84",
			want:  "github.com/google/go-github/v60",
		},
		// Conventional commit prefix: "fix(deps): update dependency @scoped/pkg"
		{
			name:  "conventional commit update dependency scoped npm",
			title: "fix(deps): update dependency @types/node to v20.10.0",
			want:  "@types/node",
		},
		// Dependabot: "Bump X from Y to Z"
		{
			name:  "dependabot bump from to",
			title: "Bump lodash from 4.17.20 to 4.17.21",
			want:  "lodash",
		},
		{
			name:  "dependabot bump scoped package",
			title: "Bump github.com/spf13/cobra from 1.8.0 to 1.9.1",
			want:  "github.com/spf13/cobra",
		},
		{
			name:  "dependabot bump with conventional commit prefix",
			title: "build(deps): bump go.opentelemetry.io/otel/sdk from 1.39.0 to 1.40.0 in the go_modules group across 1 directory",
			want:  "go.opentelemetry.io/otel/sdk",
		},
		{
			name:  "dependabot bump npm scoped package",
			title: "chore(deps-dev): bump @types/node from 25.0.3 to 25.0.9 in /backend",
			want:  "@types/node",
		},
		// Dependabot: "Bump the X group"
		{
			name:  "dependabot bump group",
			title: "Bump the go-deps group across 3 directories with 5 updates",
			want:  "go-deps",
		},
		{
			name:  "dependabot bump group with updates",
			title: "Bump the npm-production group with 2 updates",
			want:  "npm-production",
		},
		{
			name:  "dependabot bump group with conventional commit",
			title: "chore(deps): bump the npm_and_yarn group across 1 directory with 3 updates",
			want:  "npm_and_yarn",
		},
		// Dependabot: "Bump X to Y"
		{
			name:  "dependabot bump to",
			title: "Bump actions/checkout to v4",
			want:  "actions/checkout",
		},
		// Renovate onboarding
		{
			name:  "configure renovate",
			title: "Configure Renovate",
			want:  "Configure Renovate",
		},
		// Dependabot onboarding
		{
			name:  "dependabot set package ecosystem gomod",
			title: "Set package ecosystem to 'gomod' in dependabot config",
			want:  "gomod",
		},
		{
			name:  "dependabot set package-ecosystem gomod",
			title: "Set package-ecosystem to 'gomod' in dependabot.yml",
			want:  "gomod",
		},
		{
			name:  "dependabot set package ecosystem cargo",
			title: "Set package ecosystem to 'cargo' in dependabot config",
			want:  "cargo",
		},
		// Renovate: grouped update without version
		{
			name:  "renovate grouped update major",
			title: "Update github-actions (major)",
			want:  "github-actions",
		},
		{
			name:  "renovate grouped update minor",
			title: "Update npm (minor)",
			want:  "npm",
		},
		{
			name:  "renovate grouped update patch",
			title: "Update docker (patch)",
			want:  "docker",
		},
		{
			name:  "renovate grouped update digest",
			title: "Update github-actions (digest)",
			want:  "github-actions",
		},
		// Conventional commit prefix without deps scope
		{
			name:  "chore prefix configure renovate",
			title: "chore: Configure Renovate",
			want:  "Configure Renovate",
		},
		{
			name:  "build prefix bump",
			title: "build: Bump lodash from 4.17.20 to 4.17.21",
			want:  "lodash",
		},
		{
			name:  "chore(ci) prefix configure renovate",
			title: "chore(ci): Configure Renovate",
			want:  "Configure Renovate",
		},
		// Edge cases
		{
			name:  "empty title",
			title: "",
			want:  "",
		},
		{
			name:  "unrecognized title",
			title: "Fix a bug in the widget",
			want:  "",
		},
		{
			name:  "whitespace-padded title",
			title: "  Update dependency foo to v1.0.0  ",
			want:  "foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractDependencyName(tt.title)
			if got != tt.want {
				t.Errorf("ExtractDependencyName(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestExtractVersion(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{
			name:  "from to version range",
			title: "Bump lodash from 4.17.20 to 4.17.21",
			want:  "4.17.20 -> 4.17.21",
		},
		{
			name:  "from to with v prefix",
			title: "Bump k8s.io/apimachinery from v0.33.3 to v0.35.2",
			want:  "v0.33.3 -> v0.35.2",
		},
		{
			name:  "to version at end",
			title: "Update module github.com/spf13/cobra to v1.10.2",
			want:  "v1.10.2",
		},
		{
			name:  "to version with major annotation",
			title: "chore(deps): update eslint monorepo to v10 (major)",
			want:  "v10",
		},
		{
			name:  "conventional commit prefix with from to",
			title: "build(deps): bump go.opentelemetry.io/otel/sdk from 1.39.0 to 1.40.0 in the go_modules group across 1 directory",
			want:  "1.39.0 -> 1.40.0",
		},
		{
			name:  "rust crate with conventional commit",
			title: "fix(deps): update rust crate kube to v3",
			want:  "v3",
		},
		{
			name:  "no version",
			title: "fix(deps): update opentelemetry-go monorepo",
			want:  "",
		},
		{
			name:  "no version group update",
			title: "fix(deps): update all non-major dependencies",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractVersion(tt.title)
			if got != tt.want {
				t.Errorf("ExtractVersion(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestIsDependencyUpdateTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  bool
	}{
		// Conventional commit with deps scope (self-hosted Renovate)
		{
			name:  "chore(deps) update",
			title: "chore(deps): update eslint monorepo to v10 (major)",
			want:  true,
		},
		{
			name:  "fix(deps) update",
			title: "fix(deps): update dependency foo to v2",
			want:  true,
		},
		{
			name:  "chore(deps) pin",
			title: "chore(deps): pin dependencies",
			want:  true,
		},
		{
			name:  "chore(deps) terraform",
			title: "chore(deps): update terraform aws to v6",
			want:  true,
		},
		{
			name:  "chore(deps) terraform ignition",
			title: "chore(deps): update terraform ignition to v2",
			want:  true,
		},
		// Standard Renovate titles (app-authored)
		{
			name:  "update dependency",
			title: "Update dependency github.com/foo/bar to v1.2.3",
			want:  true,
		},
		{
			name:  "update module",
			title: "Update module github.com/aws/aws-sdk-go to v1.44.0",
			want:  true,
		},
		{
			name:  "lock file maintenance",
			title: "Lock file maintenance",
			want:  true,
		},
		// Standard Dependabot titles
		{
			name:  "bump from to",
			title: "Bump lodash from 4.17.20 to 4.17.21",
			want:  true,
		},
		{
			name:  "bump group",
			title: "Bump the go-deps group across 3 directories with 5 updates",
			want:  true,
		},
		// Onboarding PRs
		{
			name:  "configure renovate",
			title: "Configure Renovate",
			want:  true,
		},
		{
			name:  "dependabot set package ecosystem",
			title: "Set package ecosystem to 'gomod' in dependabot config",
			want:  true,
		},
		// Renovate grouped updates
		{
			name:  "renovate grouped update major",
			title: "Update github-actions (major)",
			want:  true,
		},
		{
			name:  "renovate grouped update minor",
			title: "Update npm (minor)",
			want:  true,
		},
		// Conventional commit prefix without deps scope
		{
			name:  "chore prefix configure renovate",
			title: "chore: Configure Renovate",
			want:  true,
		},
		// Non-dependency PRs
		{
			name:  "regular PR",
			title: "Fix a bug in the widget",
			want:  false,
		},
		{
			name:  "feature PR",
			title: "Add new authentication flow",
			want:  false,
		},
		{
			name:  "chore without deps scope",
			title: "chore: clean up CI config",
			want:  false,
		},
		{
			name:  "empty title",
			title: "",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDependencyUpdateTitle(tt.title)
			if got != tt.want {
				t.Errorf("IsDependencyUpdateTitle(%q) = %v, want %v", tt.title, got, tt.want)
			}
		})
	}
}

func TestExtractTargetVersion(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Bump lodash from 4.17.20 to 4.17.21", "4.17.21"},
		{"Bump k8s.io/apimachinery from v0.33.3 to v0.35.2", "v0.35.2"},
		{"chore(deps): update dependency typescript to v7", "v7"},
		{"chore(deps): update eslint monorepo to v10 (major)", "v10"},
		{"fix(deps): update all non-major dependencies", ""},
		{"Lock file maintenance", ""},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := ExtractTargetVersion(tt.title); got != tt.want {
				t.Errorf("ExtractTargetVersion(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
