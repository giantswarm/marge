package pr

import (
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

func TestClassifyChartSync(t *testing.T) {
	file := func(name, patch string) *github.CommitFile {
		return &github.CommitFile{Filename: new(name), Patch: new(patch)}
	}
	parent := file("helm/kagent/Chart.yaml", "@@ -3,7 +3,7 @@\n-appVersion: 0.10.1\n+appVersion: 0.10.2\n dependencies:\n   - name: kagent-tools\n-    version: 0.10.1\n+    version: 0.10.2\n")
	sub := file("helm/kagent/charts/k8s-agent/Chart.yaml", "@@ -1,3 +1,3 @@\n-version: 0.10.1\n+version: \"0.10.2\"\n")
	lock := file("vendir.lock.yml", "@@ -1 +1 @@\n-  ref: v0.10.1\n+  ref: v1.0.0\n")

	require.Equal(t, UpdatePatch, ClassifyChartSync([]*github.CommitFile{parent, sub, lock}), "only Chart.yaml files count")
	require.Equal(t, UpdateMinor, ClassifyChartSync([]*github.CommitFile{parent,
		file("helm/kagent-crds/Chart.yaml", "@@ -1 +1 @@\n-appVersion: 0.10.2\n+appVersion: 0.11.0\n")}), "the largest change of the PR")
	require.Equal(t, UpdateMajor, ClassifyChartSync([]*github.CommitFile{
		file("helm/valkey/Chart.yaml", "@@ -1,2 +1,2 @@\n-  - version: 0.8.1\n+  - version: 1.0.0\n")}))

	require.Equal(t, UpdateUnknown, ClassifyChartSync(nil), "no file")
	require.Equal(t, UpdateUnknown, ClassifyChartSync([]*github.CommitFile{lock}), "no Chart.yaml changed")
	require.Equal(t, UpdateUnknown, ClassifyChartSync([]*github.CommitFile{parent, {Filename: new("helm/x/Chart.yaml")}}), "a patch GitHub did not deliver")
	require.Equal(t, UpdateUnknown, ClassifyChartSync([]*github.CommitFile{
		file("helm/x/Chart.yaml", "@@ -1,2 +1,3 @@\n-version: 1.0.0\n+version: 1.0.1\n+    version: 2.0.0\n")}), "an added dependency pairs with nothing")
	require.Equal(t, UpdateUnknown, ClassifyChartSync([]*github.CommitFile{
		file("helm/x/Chart.yaml", "@@ -1 +1 @@\n-version: main\n+version: next\n")}), "no semver")
}

// Another team's sync PR never merges on the company defaults: the kind is
// opt-in per team.
func TestCompanyDefaultsMergeNoUpstreamSync(t *testing.T) {
	for _, u := range []UpdateType{UpdatePatch, UpdateMinor, UpdateMajor} {
		require.False(t, CompanyDefaults().Eligible(KindUpstreamSync, u), u)
	}
}
