package cmd

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/process"
)

func TestParseTeamFile(t *testing.T) {
	content := "# yaml-language-server: $schema=../.github/repositories.schema.json\n- name: agent\n  system: agent-platform\n  gen:\n    flavours: [app]\n- name: \" marge \"\n- system: no-name\n"
	got, err := parseTeamFile(content, "giantswarm", "repositories/team-bumblebee.yaml")
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/agent", "giantswarm/marge"}, got)

	_, err = parseTeamFile("- system: only\n", "o", "p")
	require.ErrorContains(t, err, "lists no repositories")

	_, err = parseTeamFile("not: [valid", "o", "p")
	require.ErrorContains(t, err, "parsing team file")
}

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

func TestTeamRepos(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/giantswarm/github/contents/repositories/team-bumblebee.yaml", func(w http.ResponseWriter, _ *http.Request) {
		body := base64.StdEncoding.EncodeToString([]byte("- name: marge\n- name: muster\n"))
		_ = json.NewEncoder(w).Encode(github.RepositoryContent{Type: new("file"), Encoding: new("base64"), Content: new(body)})
	})
	mux.HandleFunc("GET /repos/giantswarm/github/contents/repositories/team-nobody.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /repos/my-org/teams/contents/repositories/team-bumblebee.yaml", func(w http.ResponseWriter, _ *http.Request) {
		body := base64.StdEncoding.EncodeToString([]byte("- name: marge\n"))
		_ = json.NewEncoder(w).Encode(github.RepositoryContent{Type: new("file"), Encoding: new("base64"), Content: new(body)})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)

	t.Setenv(teamFileRepoEnv, "")
	got, err := RunOptions{Team: "bumblebee"}.repoList(t.Context(), client)
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/marge", "giantswarm/muster"}, got)

	_, err = teamRepos(t.Context(), client, "nobody")
	require.ErrorContains(t, err, `no team file for "nobody"`)

	t.Setenv(teamFileRepoEnv, "my-org/teams")
	got, err = teamRepos(t.Context(), client, "bumblebee")
	require.NoError(t, err)
	require.Equal(t, []string{"my-org/marge"}, got, "repositories live under the team-file repository's owner")
}

// TestResolveSweepOptions guards the two scopes and the options that follow
// from them: no wait for checks unless --check-timeout asks, and json
// output implies a quiet run.
func TestResolveSweepOptions(t *testing.T) {
	reset := func() {
		sweepFlags.actions, sweepFlags.output, sweepFlags.checkTimeout = "", "table", 0
	}

	tests := []struct {
		name    string
		opts    RunOptions
		setup   func()
		wantErr string
		check   func(t *testing.T, opts RunOptions)
	}{
		{name: "team and query are exclusive", opts: RunOptions{Team: "t", Query: "q"}, wantErr: "mutually exclusive"},
		{name: "team refuses org", opts: RunOptions{Team: "t", Org: "o"}, wantErr: "belong to the query scope"},
		{name: "team refuses repos file", opts: RunOptions{Team: "t", ReposFile: "f"}, wantErr: "belong to the query scope"},
		{name: "one scope is required", opts: RunOptions{}, wantErr: "one of --team or --query is required"},
		{name: "unknown action", opts: RunOptions{Team: "t"}, setup: func() { sweepFlags.actions = "rescue" }, wantErr: "unknown action"},
		{name: "unknown output", opts: RunOptions{Team: "t"}, setup: func() { sweepFlags.output = "yaml" }, wantErr: "unknown output"},
		{name: "team runs every action without a wait", opts: RunOptions{Team: "t"}, check: func(t *testing.T, opts RunOptions) {
			require.Zero(t, opts.CheckTimeout)
			require.True(t, opts.Actions.Has(process.ActionMerge))
			require.False(t, opts.Quiet)
		}},
		{name: "an explicit timeout is honoured", opts: RunOptions{Query: "q"}, setup: func() { sweepFlags.checkTimeout = 30 * time.Second }, check: func(t *testing.T, opts RunOptions) {
			require.Equal(t, 30*time.Second, opts.CheckTimeout)
		}},
		{name: "query does not wait either", opts: RunOptions{Query: "q"}, check: func(t *testing.T, opts RunOptions) {
			require.Zero(t, opts.CheckTimeout)
		}},
		{name: "org alone is the query scope", opts: RunOptions{Org: "o"}, check: func(t *testing.T, opts RunOptions) {
			require.Zero(t, opts.CheckTimeout)
		}},
		{name: "json output is quiet", opts: RunOptions{Team: "t"}, setup: func() { sweepFlags.output = "json"; sweepFlags.actions = "classify,approve" }, check: func(t *testing.T, opts RunOptions) {
			require.True(t, opts.Quiet)
			require.True(t, opts.NoTUI)
			require.True(t, opts.Actions.Has(process.ActionApprove))
			require.False(t, opts.Actions.Has(process.ActionMerge))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reset()
			t.Cleanup(reset)
			if tt.setup != nil {
				tt.setup()
			}
			opts := tt.opts
			err := resolveSweepOptions(&opts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, opts)
		})
	}
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
