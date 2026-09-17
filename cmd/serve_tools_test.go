package cmd

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/process"
)

// call builds a tool call with the given arguments.
func call(arguments map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: arguments}}
}

// cliOptions returns the RunOptions the sweep command builds from its flags,
// with the flags restored afterwards. It is the other side of every
// comparison below: what a person running the CLI would get.
func cliOptions(t *testing.T, opts RunOptions, actions string) RunOptions {
	t.Helper()
	previousActions, previousOutput := sweepFlags.actions, sweepFlags.output
	t.Cleanup(func() { sweepFlags.actions, sweepFlags.output = previousActions, previousOutput })
	sweepFlags.actions = actions
	// The MCP result is JSON, so the equivalent invocation is the one that
	// prints JSON: it is what makes the CLI quiet too.
	sweepFlags.output = "json"
	require.NoError(t, resolveSweepOptions(&opts))
	return opts
}

// TestTools_areTheEquivalentEngineCall is the contract of the served mode:
// a tool call runs the sweep engine with the same scope, the same steps and
// the same dry-run setting as the equivalent CLI invocation. run() is shared
// by every sweep-shaped tool, so equal requests are the same engine call.
func TestTools_areTheEquivalentEngineCall(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]any
		build     func(mcp.CallToolRequest) (sweepRequest, error)
		// cli is the equivalent `marge sweep` invocation: its flags, and
		// the --actions value.
		cli     RunOptions
		actions string
	}{
		{
			name:      "list is the classify step on a dry run",
			arguments: map[string]any{"team": "bumblebee"},
			build:     listRequest,
			cli:       RunOptions{Team: "bumblebee", DryRun: true},
			actions:   "classify",
		},
		{
			name:      "list narrowed to a few PRs",
			arguments: map[string]any{"team": "bumblebee", "prs": []any{"giantswarm/marge#7"}},
			build:     listRequest,
			cli:       RunOptions{Team: "bumblebee", DryRun: true, PRs: []string{"giantswarm/marge#7"}},
			actions:   "classify",
		},
		{
			name:      "sweep passes its scope and steps through",
			arguments: map[string]any{"team": "bumblebee", "actions": "classify,merge", "dry_run": true},
			build:     parseSweepRequest,
			cli:       RunOptions{Team: "bumblebee", DryRun: true},
			actions:   "classify,merge",
		},
		{
			name:      "sweep defaults to every step and writes",
			arguments: map[string]any{"query": "typescript"},
			build:     parseSweepRequest,
			cli:       RunOptions{Query: "typescript"},
			actions:   "",
		},
		{
			name:      "remedy is classify, remedy and mark on one PR",
			arguments: map[string]any{"pr_url": "https://github.com/giantswarm/marge/pull/7"},
			build:     remedyRequest,
			cli:       RunOptions{PRs: []string{"https://github.com/giantswarm/marge/pull/7"}},
			actions:   "classify,remedy,mark",
		},
		{
			name:      "remedy carries the dry run",
			arguments: map[string]any{"pr_url": "giantswarm/marge#7", "dry_run": true},
			build:     remedyRequest,
			cli:       RunOptions{PRs: []string{"giantswarm/marge#7"}, DryRun: true},
			actions:   "classify,remedy,mark",
		},
		{
			name:      "remedy under a team is decided by that team's policy",
			arguments: map[string]any{"pr_url": "giantswarm/marge#7", "team": "bumblebee"},
			build:     remedyRequest,
			cli:       RunOptions{PRs: []string{"giantswarm/marge#7"}, Team: "bumblebee"},
			actions:   "classify,remedy,mark",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := tt.build(call(tt.arguments))
			require.NoError(t, err)

			want := cliOptions(t, tt.cli, tt.actions)
			require.Equal(t, want, req.Opts)
		})
	}
}

// TestScopeRepos holds where a run narrowed to a few PRs reads from: the
// repositories those PRs live in, never all of GitHub. A scope that names
// its repositories keeps them.
func TestScopeRepos(t *testing.T) {
	tests := []struct {
		name   string
		scoped []string
		refs   []string
		want   []string
	}{
		{"no scope and no PRs searches GitHub", nil, nil, nil},
		{"a scope wins", []string{"giantswarm/muster"}, []string{"giantswarm/marge#7"}, []string{"giantswarm/muster"}},
		{"one PR names its repository", nil, []string{"giantswarm/marge#7"}, []string{"giantswarm/marge"}},
		{"an URL names its repository", nil, []string{"https://github.com/giantswarm/marge/pull/7"}, []string{"giantswarm/marge"}},
		{"two PRs of one repository name it once", nil, []string{"giantswarm/marge#7", "giantswarm/marge#8"}, []string{"giantswarm/marge"}},
		{"two repositories are both read", nil, []string{"giantswarm/marge#7", "giantswarm/muster#8"}, []string{"giantswarm/marge", "giantswarm/muster"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scopeRepos(tt.scoped, tt.refs)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	_, err := scopeRepos(nil, []string{"not a reference"})
	require.Error(t, err)
}

// TestTools_annotations holds what a client is told before it calls: only
// list is read-only, and the two tools that change a PR are destructive.
// mcp-go fills an unset hint with the specification's default, so every
// hint is spelled out and every one of them is checked.
func TestTools_annotations(t *testing.T) {
	tests := []struct {
		tool        mcp.Tool
		readOnly    bool
		destructive bool
	}{
		{tool: listTool(), readOnly: true, destructive: false},
		{tool: sweepTool(), readOnly: false, destructive: true},
		{tool: remedyTool(), readOnly: false, destructive: true},
		{tool: markTool(), readOnly: false, destructive: false},
	}

	for _, tt := range tests {
		t.Run(tt.tool.Name, func(t *testing.T) {
			annotations := tt.tool.Annotations
			require.NotNil(t, annotations.ReadOnlyHint, "readOnlyHint must be declared, not defaulted")
			require.NotNil(t, annotations.DestructiveHint, "destructiveHint must be declared, not defaulted")
			require.NotNil(t, annotations.IdempotentHint, "idempotentHint must be declared, not defaulted")
			require.NotNil(t, annotations.OpenWorldHint, "openWorldHint must be declared, not defaulted")
			require.Equal(t, tt.readOnly, *annotations.ReadOnlyHint)
			require.Equal(t, tt.destructive, *annotations.DestructiveHint)
			require.True(t, *annotations.OpenWorldHint, "every tool reads GitHub, which is outside this server")
		})
	}
}

// TestWritingTools_takeADryRun holds the rule that every tool which can
// change a PR offers a preview of what it would change.
func TestWritingTools_takeADryRun(t *testing.T) {
	for _, tool := range []mcp.Tool{sweepTool(), remedyTool(), markTool()} {
		t.Run(tool.Name, func(t *testing.T) {
			require.Contains(t, tool.InputSchema.Properties, "dry_run")
		})
	}
}

// TestTools_withoutAGrantAskForASignIn is the failure mode this server must
// never have: a call that carries no caller token is refused with the
// sign-in error, on every tool, instead of running as whatever identity the
// process happens to hold. The GitHub client is built before any argument is
// used, so a call with no arguments at all reaches the same refusal.
func TestTools_withoutAGrantAskForASignIn(t *testing.T) {
	tools := toolset{newClient: gh.NewCallerClient}
	handlers := map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"list":   tools.handleList,
		"sweep":  tools.handleSweep,
		"remedy": tools.handleRemedy,
		"mark":   tools.handleMark,
	}

	arguments := map[string]any{"team": "bumblebee", "pr_url": "giantswarm/marge#7"}
	for name, handle := range handlers {
		t.Run(name, func(t *testing.T) {
			got, err := handle(t.Context(), call(arguments))
			require.NoError(t, err, "a missing grant is a tool error, not a transport error")
			require.True(t, got.IsError)
			require.Contains(t, textOf(t, got), "Sign in")
		})
	}
}

// TestStdioServesTheLocalCredential_httpServesTheCaller pins which client
// each transport acts with: a stdio server is started by the person using
// it, a served one acts as whoever called it and never as the process.
func TestStdioServesTheLocalCredential_httpServesTheCaller(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "a-token-of-the-process")

	local := toolset{newClient: gh.NewClient}
	client, err := local.newClient(t.Context())
	require.NoError(t, err)
	require.NotNil(t, client)

	served := toolset{newClient: gh.NewCallerClient}
	_, err = served.newClient(t.Context())
	require.ErrorIs(t, err, gh.ErrNoCallerToken)

	_, err = served.newClient(gh.ContextWithCallerToken(t.Context(), "the-callers-grant"))
	require.NoError(t, err)
}

// TestRemedyRequest_rejectsAReferenceThatIsNotAPR keeps a typo from becoming
// a sweep of the whole scope.
func TestRemedyRequest_rejectsAReferenceThatIsNotAPR(t *testing.T) {
	for _, ref := range []string{"", "giantswarm/marge", "not a reference", "https://github.com/giantswarm/marge"} {
		_, err := remedyRequest(call(map[string]any{"pr_url": ref}))
		require.Error(t, err, "reference %q must be refused", ref)
	}
}

// TestListRequest_isAlwaysReadOnly holds that no argument turns the
// read-only tool into one that writes.
func TestListRequest_isAlwaysReadOnly(t *testing.T) {
	req, err := listRequest(call(map[string]any{"team": "bumblebee", "dry_run": false, "actions": "merge"}))
	require.NoError(t, err)
	require.True(t, req.Opts.DryRun)
	require.Equal(t, process.ActionSet{process.ActionClassify: true}, req.Opts.Actions)
}

// TestScopeArgumentsAreExclusive holds the same rule the CLI enforces: a
// call never names a team and a query scope together.
func TestScopeArgumentsAreExclusive(t *testing.T) {
	for _, build := range []func(mcp.CallToolRequest) (sweepRequest, error){listRequest, parseSweepRequest} {
		_, err := build(call(map[string]any{"team": "bumblebee", "query": "typescript"}))
		require.Error(t, err)
	}
}

// textOf returns the text content of a tool result.
func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "tool result is not text content")
	return text.Text
}
