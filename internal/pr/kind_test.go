package pr

import (
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

func TestKindOf(t *testing.T) {
	require.Equal(t, KindRenovate, KindOf("renovate[bot]", nil))
	require.Equal(t, KindAlignFiles, KindOf("giantswarm-align-files[bot]", nil))
	require.Equal(t, KindHerald, KindOf("heraldbot[bot]", nil))
	require.Equal(t, KindDependabot, KindOf("dependabot[bot]", nil))
	require.Equal(t, Kind(""), KindOf("renovate", nil))
	require.Equal(t, Kind(""), KindOf("quentin", nil))
	require.Equal(t, Kind(""), KindOf("", nil))
	require.Equal(t, KindUpstreamSync, KindOf("taylorbot", []string{"dependencies", "automated-update"}))
	require.Equal(t, Kind(""), KindOf("taylorbot", []string{"dependencies"}), "taylorbot's other PRs carry no sync label")
	require.Equal(t, Kind(""), KindOf("quentin", []string{"automated-update"}), "the label alone is not a sync PR")
	require.Equal(t, []string{"dependabot[bot]", "giantswarm-align-files[bot]", "heraldbot[bot]", "renovate[bot]"}, TrustedLogins())
	require.Equal(t, []string{"author:app/dependabot", "author:app/giantswarm-align-files", "author:app/heraldbot", "author:app/renovate", "author:taylorbot label:automated-update"}, SearchQualifiers())
}

func TestLeftToClassification(t *testing.T) {
	require.True(t, LeftToClassification(KindAlignFiles, "giantswarm", "github", "reposetup/rename-old-name"))
	require.True(t, LeftToClassification(KindAlignFiles, "GiantSwarm", "GitHub", "reposetup/rename-old-name"), "GitHub names are case-insensitive")
	require.False(t, LeftToClassification(KindAlignFiles, "giantswarm", "github", "teams-alignment-branch"), "a regular Align files PR")
	require.False(t, LeftToClassification(KindAlignFiles, "giantswarm", "marge", "reposetup/codeowners"), "another repository")
	require.False(t, LeftToClassification(KindRenovate, "giantswarm", "github", "reposetup/rename-old-name"), "another bot")
}

func TestClassifyUpdate(t *testing.T) {
	renovateBody := func(from, to string) string {
		return "This PR contains the following updates:\n\n| Package | Change | Age |\n|---|---|---|\n| [foo](https://x) | `" + from + "` → `" + to + "` | ![age](x) |\n"
	}
	tests := []struct {
		name  string
		kind  Kind
		title string
		body  string
		want  UpdateType
	}{
		{"align files has no version", KindAlignFiles, "chore: align files according to platform standards", "", UpdateNone},
		{"herald has no version", KindHerald, "fix(nancy): remediate findings on main", "", UpdateNone},
		{"renovate patch from body", KindRenovate, "fix(deps): update module github.com/aws/aws-sdk-go-v2/config to v1.33.5", renovateBody("v1.33.4", "v1.33.5"), UpdatePatch},
		{"renovate minor from body", KindRenovate, "chore(deps): update helm release kube-state-metrics to v8.5.0", renovateBody("8.4.1", "8.5.0"), UpdateMinor},
		{"renovate major from body", KindRenovate, "fix(deps): update module sigs.k8s.io/cluster-api to v2", renovateBody("v1.14.2", "v2.0.0"), UpdateMajor},
		{"renovate go major module path without body", KindRenovate, "fix(deps): update module github.com/google/go-github/v91 to v92", "", UpdateMajor},
		{"renovate go same major path without body is unknown", KindRenovate, "fix(deps): update module github.com/google/go-github/v92 to v92.1.0", "", UpdateUnknown},
		{"renovate npm range major", KindRenovate, "chore(deps): update dependency typescript to v7", "| [typescript](https://x) | [`~6.0.0` → `~7.0.0`](https://renovatebot.com/diffs/npm/typescript/6.0.3/7.0.2) |", UpdateMajor},
		{"renovate npm caret minor", KindRenovate, "chore(deps): update dependency react to v19.2", renovateBody("^19.1.0", "^19.2.0"), UpdateMinor},
		{"renovate digest", KindRenovate, "chore(deps): update ghcr.io/foo/bar docker digest to 3f1c2ab", renovateBody("9a8b7c6d5e4f", "3f1c2ab4d5e6"), UpdateDigest},
		{"renovate grouped takes the largest row", KindRenovate, "chore(deps): update codemirror", renovateBody("6.0.1", "6.0.2") + renovateBody("6.1.0", "7.0.0"), UpdateMajor},
		{"renovate grouped type in title", KindRenovate, "chore(deps): update github-actions (minor)", "", UpdateMinor},
		{"renovate non-major group", KindRenovate, "chore(deps): update all non-major dependencies", "", UpdateMinor},
		{"renovate lockfile", KindRenovate, "chore(deps): lock file maintenance", "", UpdateLockfile},
		{"renovate pin", KindRenovate, "chore(deps): pin dependencies", "", UpdatePin},
		{"renovate without versions is unknown", KindRenovate, "chore(deps): update codemirror", "no table here", UpdateUnknown},
		{"renovate unreadable versions are unknown", KindRenovate, "chore(deps): update foo to latest", renovateBody("latest", "stable"), UpdateUnknown},
		{"dependabot patch from title", KindDependabot, "Bump github.com/containerd/containerd from 1.7.31 to 1.7.35 in /tests/e2e", "", UpdatePatch},
		{"dependabot minor from title", KindDependabot, "chore(deps): bump actions/checkout from 4.1.0 to 4.2.0", "", UpdateMinor},
		{"dependabot major from title", KindDependabot, "Bump lodash from 3.10.1 to 4.17.21", "", UpdateMajor},
		{"dependabot group without versions is unknown", KindDependabot, "Bump the go_modules group across 1 directory with 3 updates", "", UpdateUnknown},
		{"dependabot group takes the largest body row", KindDependabot, "Bump the go_modules group across 1 directory with 3 updates", "Bumps the go_modules group with 3 updates in the / directory: a, b and c.\n\nUpdates `github.com/a/a` from 1.2.3 to 1.2.4\n- [Release notes](https://x)\n\nUpdates `github.com/b/b` from 2.1.0 to 2.3.0\n\nUpdates `github.com/c/c` from 0.9.1 to 0.9.2\n", UpdateMinor},
		{"dependabot group with a major row is major", KindDependabot, "Bump the npm_and_yarn group with 2 updates", "Updates `lodash` from 3.10.1 to 4.17.21\nUpdates `react` from 18.3.1 to 18.3.2\n", UpdateMajor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ClassifyUpdate(tt.kind, tt.title, tt.body))
		})
	}
}

// TestKindOfPull_selfHostedRenovate: a Renovate running under the account
// that owns a personal repository opens its PRs as that account. Such a PR
// is Renovate's when it comes from a Renovate branch and carries Renovate's
// signature; anything short of all three is a person's PR.
func TestKindOfPull_selfHostedRenovate(t *testing.T) {
	const signed = "Update x.\n\nThis PR has been generated by Renovate.\n<!--renovate-debug:eyJ9-->"
	pull := func(login, head, body string) *github.PullRequest {
		return &github.PullRequest{User: &github.User{Login: new(login)}, Head: &github.PullRequestBranch{Ref: new(head)}, Body: new(body)}
	}
	require.Equal(t, KindRenovate, KindOfPull("jane", pull("jane", "renovate/x-1.x", signed)))
	require.Equal(t, KindRenovate, KindOfPull("Jane", pull("jane", "renovate/x-1.x", signed)), "GitHub logins are case-insensitive")
	require.Equal(t, KindRenovate, KindOfPull("jane", pull("jane", "renovate/configure", "Welcome to Renovate!\n<!--renovate-config-hash:94693a-->")), "an onboarding PR")
	require.Equal(t, Kind(""), KindOfPull("jane", pull("jane", "renovate/x-1.x", "Update x.")), "no Renovate signature")
	require.Equal(t, Kind(""), KindOfPull("jane", pull("jane", "feature/renovate", signed)), "not a Renovate branch")
	require.Equal(t, Kind(""), KindOfPull("jane", pull("quentin", "renovate/x-1.x", signed)), "a collaborator is not the owner")
	require.Equal(t, Kind(""), KindOfPull("org", pull("quentin", "renovate/x-1.x", signed)), "an organisation repository has no owner-authored PRs")
	require.Equal(t, KindRenovate, KindOfPull("org", pull("renovate[bot]", "renovate/x-1.x", "")), "the Renovate App needs no signature")
}

func TestDiscoveredKind(t *testing.T) {
	require.Equal(t, KindRenovate, DiscoveredKind(PRInfo{Owner: "jane", Author: "jane", HeadRef: "renovate/x"}))
	require.Equal(t, KindDependabot, DiscoveredKind(PRInfo{Owner: "org", Author: "dependabot[bot]"}))
	require.Equal(t, Kind(""), DiscoveredKind(PRInfo{Owner: "jane", Author: "jane", HeadRef: "fix-bug"}))
	require.Equal(t, Kind(""), DiscoveredKind(PRInfo{Owner: "org", Author: "quentin", HeadRef: "renovate/x"}))
}
