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
	got, err := parseTeamFile(content, "repositories/team-bumblebee.yaml")
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/agent", "giantswarm/marge"}, got)

	_, err = parseTeamFile("- system: only\n", "p")
	require.ErrorContains(t, err, "lists no repositories")

	_, err = parseTeamFile("not: [valid", "p")
	require.ErrorContains(t, err, "parsing team file")
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
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)

	got, err := RunOptions{Team: "bumblebee"}.repoList(t.Context(), client)
	require.NoError(t, err)
	require.Equal(t, []string{"giantswarm/marge", "giantswarm/muster"}, got)

	_, err = teamRepos(t.Context(), client, "nobody")
	require.ErrorContains(t, err, `no team file for "nobody"`)
}

// TestResolveSweepOptions guards the two scopes and the options that follow
// from them: the team scope waits zero for checks unless asked, the query
// scope waits the default, and json output implies a quiet run.
func TestResolveSweepOptions(t *testing.T) {
	newCmd := func(args ...string) *cobra.Command {
		cmd := &cobra.Command{}
		cmd.Flags().Duration("check-timeout", 0, "")
		require.NoError(t, cmd.Flags().Parse(args))
		return cmd
	}
	reset := func() {
		sweepFlags.actions, sweepFlags.output, sweepFlags.checkTimeout = "", "table", 0
	}

	tests := []struct {
		name    string
		opts    RunOptions
		args    []string
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
		{name: "team waits zero", opts: RunOptions{Team: "t"}, check: func(t *testing.T, opts RunOptions) {
			require.Zero(t, opts.CheckTimeout)
			require.True(t, opts.Actions.Has(process.ActionMerge))
			require.False(t, opts.Quiet)
		}},
		{name: "team honours an explicit timeout", opts: RunOptions{Team: "t"}, args: []string{"--check-timeout=30s"}, setup: func() { sweepFlags.checkTimeout = 30 * time.Second }, check: func(t *testing.T, opts RunOptions) {
			require.Equal(t, 30*time.Second, opts.CheckTimeout)
		}},
		{name: "query waits the default", opts: RunOptions{Query: "q"}, check: func(t *testing.T, opts RunOptions) {
			require.Equal(t, defaultQueryCheckTimeout, opts.CheckTimeout)
		}},
		{name: "org alone is the query scope", opts: RunOptions{Org: "o"}, check: func(t *testing.T, opts RunOptions) {
			require.Equal(t, defaultQueryCheckTimeout, opts.CheckTimeout)
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
			err := resolveSweepOptions(newCmd(tt.args...), &opts)
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
