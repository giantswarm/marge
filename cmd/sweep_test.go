package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
)

func TestTeamFileRepo(t *testing.T) {
	t.Setenv(teamFileRepoEnv, "")
	owner, name, err := teamFileRepo()
	require.NoError(t, err)
	require.Equal(t, "giantswarm", owner)
	require.Equal(t, "github", name)

	t.Setenv(teamFileRepoEnv, " my-org/teams ")
	owner, name, err = teamFileRepo()
	require.NoError(t, err)
	require.Equal(t, "my-org", owner)
	require.Equal(t, "teams", name)

	for _, bad := range []string{"my-org", "my-org/", "/teams", "a/b/c"} {
		t.Setenv(teamFileRepoEnv, bad)
		_, _, err := teamFileRepo()
		require.ErrorContains(t, err, "want owner/repo", bad)
	}
}

// contentsMux serves the files of a fake giantswarm/github over the
// contents API. A path that holds no file and no file below it answers 404,
// the way GitHub answers for a team that has no policy file. A path that has
// files below it answers with the directory listing, which is how the
// schedule finds the teams that opted in.
func contentsMux(t *testing.T, owner, repo string, files map[string]string) *github.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("GET /repos/%s/%s/contents/", owner, repo), func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/contents/", owner, repo))
		if content, ok := files[path]; ok {
			body := base64.StdEncoding.EncodeToString([]byte(content))
			_ = json.NewEncoder(w).Encode(github.RepositoryContent{Type: new("file"), Encoding: new("base64"), Content: new(body)})
			return
		}
		entries := directoryEntries(files, path)
		if len(entries) == 0 {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return client
}

// directoryEntries returns the files directly below dir, as the contents
// API lists them.
func directoryEntries(files map[string]string, dir string) []github.RepositoryContent {
	var entries []github.RepositoryContent
	for path := range files {
		name, ok := strings.CutPrefix(path, dir+"/")
		if !ok || strings.Contains(name, "/") {
			continue
		}
		entries = append(entries, github.RepositoryContent{Type: new("file"), Name: new(name), Path: new(path)})
	}
	slices.SortFunc(entries, func(a, b github.RepositoryContent) int {
		return strings.Compare(a.GetPath(), b.GetPath())
	})
	return entries
}

// TestResolveScope_teamScope reads the three files of a team scope and
// resolves them: the repository list comes from the team file and every PR
// carries the policy its repository is swept under.
func TestResolveScope_teamScope(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"bot-prs-sweep/default.yaml":        "updateTypes:\n  renovate: [patch, minor]\nschedule: disabled\n",
		"bot-prs-sweep/team-bumblebee.yaml": "slackChannel: team-bumblebee\nschedule: enabled\n",
		"repositories/team-bumblebee.yaml":  "- name: marge\n- name: muster\n  botPRsSweep:\n    updateTypes: [patch]\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	scope, err := RunOptions{Team: "bumblebee"}.resolveScope(t.Context(), client)
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/marge", "giantswarm/muster"}, scope.Repos)

	require.True(t, scope.Policies.Base().Schedule)
	require.Equal(t, "team-bumblebee", scope.Policies.Base().SlackChannel)
	require.True(t, scope.Policies.For("giantswarm", "marge").Eligible(pr.KindRenovate, pr.UpdateMinor))
	require.False(t, scope.Policies.For("giantswarm", "muster").Eligible(pr.KindRenovate, pr.UpdateMinor))
	// Scope.Repos carries the owner, so both shapes reach the exception.
	require.False(t, scope.Policies.For("giantswarm", "muster").Eligible(pr.KindRenovate, pr.UpdateMinor))
}

// TestResolveScope_noPolicyFile sweeps a team that has a repository list but
// no policy file: the company defaults apply and the schedule stays off.
func TestResolveScope_noPolicyFile(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"repositories/team-shield.yaml": "- name: cluster-aws\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	scope, err := RunOptions{Team: "shield"}.resolveScope(t.Context(), client)
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/cluster-aws"}, scope.Repos)
	require.False(t, scope.Policies.Base().Schedule)
	require.Equal(t, pr.CompanyDefaults(), scope.Policies.For("giantswarm", "cluster-aws"))
	require.Equal(t, []string{"built-in company defaults"}, scope.Policies.Base().Sources)
}

// TestResolveScope_missingTeamFile refuses to sweep a team whose repository
// list is not there: without it the sweep has no scope.
func TestResolveScope_missingTeamFile(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", nil)

	t.Setenv(teamFileRepoEnv, "")
	_, err := RunOptions{Team: "nobody"}.resolveScope(t.Context(), client)
	require.ErrorContains(t, err, `no team file for "nobody"`)
}

// TestResolveScope_malformedPolicyFile stops the sweep instead of applying
// defaults the team never wrote.
func TestResolveScope_malformedPolicyFile(t *testing.T) {
	client := contentsMux(t, "giantswarm", "github", map[string]string{
		"bot-prs-sweep/default.yaml":       "shedule: enabled\n",
		"repositories/team-bumblebee.yaml": "- name: marge\n",
	})

	t.Setenv(teamFileRepoEnv, "")
	_, err := RunOptions{Team: "bumblebee"}.resolveScope(t.Context(), client)
	require.ErrorContains(t, err, "bot-prs-sweep/default.yaml")
	require.ErrorContains(t, err, "field shedule not found")
}

// TestResolveScope_queryScope reads the company defaults alone: the query
// scope has no team, so no team file and no exception apply.
func TestResolveScope_queryScope(t *testing.T) {
	client := contentsMux(t, "my-org", "teams", map[string]string{
		"bot-prs-sweep/default.yaml": "concurrency:\n  perTeam: 2\n",
	})

	t.Setenv(teamFileRepoEnv, "my-org/teams")
	scope, err := RunOptions{Query: "org:giantswarm"}.resolveScope(t.Context(), client)
	require.NoError(t, err)
	require.Nil(t, scope.Repos, "the query scope leaves the PRs to the GitHub search")
	require.Equal(t, 2, scope.Policies.Base().Concurrency.PerTeam)

	path := filepath.Join(t.TempDir(), "repos.txt")
	require.NoError(t, os.WriteFile(path, []byte("my-org/marge\n"), 0o600))
	scope, err = RunOptions{ReposFile: path}.resolveScope(t.Context(), client)
	require.NoError(t, err)
	require.Equal(t, []string{"my-org/marge"}, scope.Repos)
}

// TestResolveSweepOptions guards the two scopes and the options that follow
// from them: no wait for checks unless --check-timeout asks, and json
// output implies a quiet run.
// TestSweepFlags_theChartPassesThese keeps the flags of a scheduled run and
// the flags the CronJob writes on one side. A renamed flag would otherwise
// fail the first time a CronJob runs, on the installation and not here.
func TestSweepFlags_theChartPassesThese(t *testing.T) {
	for _, name := range []string{"all-teams", "team", "actions", "no-tui", "dry-run", "post-summary"} {
		require.NotNil(t, sweepCmd.Flags().Lookup(name), "helm/marge/templates/cronjob.yaml passes --%s", name)
	}
}

func TestResolveSweepOptions(t *testing.T) {
	reset := func() {
		sweepFlags.actions, sweepFlags.output, sweepFlags.checkTimeout = "", "table", 0
		sweepFlags.teams = nil
	}

	tests := []struct {
		name    string
		teams   []string
		opts    RunOptions
		setup   func()
		wantErr string
		check   func(t *testing.T, teams []string, opts RunOptions)
	}{
		{name: "team and query are exclusive", teams: []string{"t"}, opts: RunOptions{Query: "q"}, wantErr: "mutually exclusive"},
		{name: "team refuses org", teams: []string{"t"}, opts: RunOptions{Org: "o"}, wantErr: "belong to the query scope"},
		{name: "team refuses repos file", teams: []string{"t"}, opts: RunOptions{ReposFile: "f"}, wantErr: "belong to the query scope"},
		{name: "several teams refuse named PRs", teams: []string{"one", "two"}, opts: RunOptions{PRs: []string{"giantswarm/marge#1"}}, wantErr: "give a single --team"},
		{name: "one scope is required", opts: RunOptions{}, wantErr: "one of --team or --query is required"},
		{name: "a blank team names no team", teams: []string{" "}, wantErr: "--team names no team"},
		{name: "unknown action", teams: []string{"t"}, setup: func() { sweepFlags.actions = "rescue" }, wantErr: "unknown action"},
		{name: "unknown output", teams: []string{"t"}, setup: func() { sweepFlags.output = "yaml" }, wantErr: "unknown output"},
		{name: "team runs every action without a wait", teams: []string{"t"}, check: func(t *testing.T, teams []string, opts RunOptions) {
			require.Equal(t, []string{"t"}, teams)
			require.Equal(t, "t", opts.Team, "one team is the single-team scope")
			require.Zero(t, opts.CheckTimeout)
			require.True(t, opts.Actions.Has(process.ActionMerge))
			require.False(t, opts.Quiet)
		}},
		{name: "several teams are kept in order and deduplicated", teams: []string{"two", "one", "two", ""}, check: func(t *testing.T, teams []string, opts RunOptions) {
			require.Equal(t, []string{"two", "one"}, teams)
			require.Empty(t, opts.Team, "no single team owns a run of several")
		}},
		{name: "all-teams refuses a named team", teams: []string{"t"}, setup: func() { sweepFlags.allTeams = true }, wantErr: "--all-teams reads every team's own scope"},
		{name: "an explicit timeout is honoured", opts: RunOptions{Query: "q"}, setup: func() { sweepFlags.checkTimeout = 30 * time.Second }, check: func(t *testing.T, _ []string, opts RunOptions) {
			require.Equal(t, 30*time.Second, opts.CheckTimeout)
		}},
		{name: "query does not wait either", opts: RunOptions{Query: "q"}, check: func(t *testing.T, _ []string, opts RunOptions) {
			require.Zero(t, opts.CheckTimeout)
		}},
		{name: "org alone is the query scope", opts: RunOptions{Org: "o"}, check: func(t *testing.T, teams []string, opts RunOptions) {
			require.Empty(t, teams)
			require.Zero(t, opts.CheckTimeout)
		}},
		{name: "json output is quiet", teams: []string{"t"}, setup: func() { sweepFlags.output = "json"; sweepFlags.actions = "classify,approve" }, check: func(t *testing.T, _ []string, opts RunOptions) {
			require.True(t, opts.Quiet)
			require.True(t, opts.NoTUI)
			require.True(t, opts.Actions.Has(process.ActionApprove))
			require.False(t, opts.Actions.Has(process.ActionMerge))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reset()
			t.Cleanup(func() { reset(); sweepFlags.allTeams = false })
			sweepFlags.teams = tt.teams
			if tt.setup != nil {
				tt.setup()
			}
			opts := tt.opts
			teams, err := resolveSweepOptions(&opts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, teams, opts)
		})
	}
}

// TestSweepTeamFlagTakesSeveralNames holds what the flag promises: one
// --team carries several names, repeated or comma-separated.
func TestSweepTeamFlagTakesSeveralNames(t *testing.T) {
	previous := sweepFlags.teams
	t.Cleanup(func() { sweepFlags.teams = previous })

	flags := sweepCmd.Flags()
	require.NoError(t, flags.Set("team", "bumblebee,atlas"))
	require.NoError(t, flags.Set("team", "phoenix"))
	require.Equal(t, []string{"bumblebee", "atlas", "phoenix"}, sweepFlags.teams)
}

// TestSweepFlagHelpNamesEveryAction keeps the --actions help in step with
// the engine's action list.
func TestSweepFlagHelpNamesEveryAction(t *testing.T) {
	for _, cmd := range []*cobra.Command{sweepCmd, runCmd} {
		usage := cmd.Flags().Lookup("actions").Usage
		for _, a := range process.ActionNames() {
			require.True(t, strings.Contains(usage, a), "%s --actions help lacks %q", cmd.Name(), a)
		}
	}
}
