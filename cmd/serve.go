package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/google/go-github/v93/github"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
)

func init() {
	serveCmd.Flags().StringVar(&serveOpts.transport, "transport", transportStdio, "Transport: stdio or streamable-http")
	serveCmd.Flags().StringVar(&serveOpts.httpAddr, "http-addr", ":8080", "Listen address for the streamable-http transport")
	rootCmd.AddCommand(serveCmd)
}

const (
	transportStdio          = "stdio"
	transportStreamableHTTP = "streamable-http"
	// mcpEndpoint is the path the streamable HTTP transport is served on;
	// /healthz and /readyz answer the Kubernetes probes next to it.
	mcpEndpoint = "/mcp"
	// shutdownGrace bounds how long a stopping server waits for in-flight
	// requests -- a sweep can run for minutes, but a terminating pod gets
	// seconds.
	shutdownGrace = 10 * time.Second
)

var serveOpts struct {
	transport string
	httpAddr  string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start an MCP server exposing the sweep as tools",
	Long: `Start a Model Context Protocol (MCP) server.
The server exposes "list", "sweep", "remedy" and "mark", which mirror the CLI
commands of those names and return structured JSON instead of terminal
output, and "changelog", which writes one changelog entry on a bot PR in the
team's own format.

Transports:
  stdio            JSON-RPC over stdin/stdout, for a local MCP client that
                   starts marge itself (default)
  streamable-http  the MCP Streamable HTTP transport on --http-addr, with the
                   endpoint at ` + mcpEndpoint + ` and liveness/readiness probes
                   at /healthz and /readyz; this is what the Helm chart runs`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// A server that finds its binary replaced cancels ctx and so stops
		// the way an interrupt would.
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, replaced := context.WithCancel(ctx)
		defer replaced()
		guard := server.WithToolHandlerMiddleware(restartGuard(watchBinary(), replaced, cmd.ErrOrStderr()))

		switch serveOpts.transport {
		case transportStdio:
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			// Listen answers a cancelled context with its error; stopping on
			// a signal or a replaced binary is a clean exit.
			err := server.NewStdioServer(newMCPServer(gh.NewClient, guard)).Listen(ctx, os.Stdin, os.Stdout)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case transportStreamableHTTP:
			return serveHTTP(ctx, newMCPServer(gh.NewCallerClient, guard), serveOpts.httpAddr, cmd.ErrOrStderr())
		default:
			return fmt.Errorf("unknown transport %q: use %s or %s", serveOpts.transport, transportStdio, transportStreamableHTTP)
		}
	},
}

// clientFactory returns the GitHub client one tool call acts with, and is
// what separates the two transports. A stdio server was started by the
// person using it, so it acts with that person's own credential; a served
// one acts with the token its caller presented and with nothing else.
type clientFactory func(context.Context) (*github.Client, error)

// toolset holds what every tool handler needs. Each tool is an adapter: it
// reads its arguments, builds one sweepRequest and hands it to run, which is
// the same engine call the sweep command makes. No tool reaches past it and
// no tool knows what kind of client it got.
type toolset struct {
	newClient clientFactory
}

// newMCPServer builds the MCP server and its tools over newClient; the
// transport is chosen by the caller.
func newMCPServer(newClient clientFactory, options ...server.ServerOption) *server.MCPServer {
	mcpServer := server.NewMCPServer(
		"marge",
		version,
		append([]server.ServerOption{server.WithToolCapabilities(true)}, options...)...,
	)

	tools := toolset{newClient: newClient}
	mcpServer.AddTool(listTool(), reportProgress(tools.handleList))
	mcpServer.AddTool(sweepTool(), reportProgress(tools.handleSweep))
	mcpServer.AddTool(remedyTool(), reportProgress(tools.handleRemedy))
	mcpServer.AddTool(markTool(), tools.handleMark)
	mcpServer.AddTool(changelogTool(), tools.handleChangelog)

	return mcpServer
}

// Every tool spells out all four annotations. mcp-go fills an unset hint
// with the specification's default, so a hint left out ships as a claim.
// openWorldHint is true throughout: every tool reads and writes GitHub,
// which is outside this server.
func listTool() mcp.Tool {
	return mcp.NewTool("list",
		tooldesc.Describe("List the open bot PRs of a team or query scope with each PR's classification, label, bot kind and age.",
			"By default it reads back the class the last sweep stored in each PR's marge/<class> label: what the last sweep decided, not what a sweep would decide now. "+
				"refresh: true runs the sweep's classify step on a dry run instead. "+
				"unclassified means no sweep has labelled that PR, not that it needs nothing: call again with refresh: true. "+
				"Entries are grouped as the sweep reports them; rescue tooling acts on action_required only and skips an entry whose rescue is not stale."),
		scopeArguments(),
		mcp.WithArray("teams",
			mcp.Description("Several teams in one call, each under its own team file and policy, with one shared discovery; answers {\"teams\": [{team, result or error}]} in order."),
			mcp.WithStringItems(),
		),
		mcp.WithBoolean("refresh",
			mcp.Description("Classify every PR now instead of reading its stored marge/<class> label (default false); costs a PR and a check read per PR, adds evidence and policy."),
		),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func remedyTool() mcp.Tool {
	return mcp.NewTool("remedy",
		tooldesc.Describe("Remedy one bot PR: classify it and apply the catalogue rule that matches, through that rule's action and its guards.",
			"The sweep's classify, remedy and mark steps on one PR, with its guards, refusals and evidence comment; a rule's action may close the PR or rewrite required checks. "+
				"Naming a rule only removes the others from the contest: it still has to match. "+
				"When nothing matches, nothing is written and the failure is reported under unhandled."),
		mcp.WithString("pr_url",
			mcp.Required(),
			mcp.Description("The pull request, as an URL (https://github.com/OWNER/REPO/pull/NUMBER) or as OWNER/REPO#NUMBER"),
		),
		mcp.WithString("rule",
			mcp.Description("Apply this rule of the catalogue instead of letting every rule compete. The rule must still match the PR."),
		),
		mcp.WithString("team",
			mcp.Description("Decide the PR under this team's policy, from bot-prs-sweep/team-<name>.yaml in the team-file repository; without it, the company default policy."),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Report the rule that would apply and write nothing (default: false)"),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func markTool() mcp.Tool {
	return mcp.NewTool("mark",
		tooldesc.Describe("Record a failed AI rescue attempt on a PR as an ai-rescue marker comment, which later sweeps report.",
			"The marker records the head SHA and a fingerprint of the diff: it goes stale when the PR content changes (new version, pushed fix), but survives a rebase that leaves the diff unchanged."),
		mcp.WithString("pr_url",
			mcp.Required(),
			mcp.Description("The pull request, as an URL (https://github.com/OWNER/REPO/pull/NUMBER) or as OWNER/REPO#NUMBER"),
		),
		mcp.WithString("outcome",
			mcp.Description("Rescue outcome (default: \"failed\")"),
			mcp.Enum("failed", "blocked"),
		),
		mcp.WithString("reason",
			mcp.Description("Short explanation of why the rescue did not succeed"),
		),
		mcp.WithString("tool",
			mcp.Description("Name of the tool/agent that attempted the rescue (default: \"ai\")"),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Return the marker that would be written, head SHA and fingerprint included, and post nothing (default: false)"),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

// maxMCPBodyBytes bounds an MCP request body. mcp-go reads a POST body whole
// before validating it (CVE-2026-108859); the bound keeps that read small.
const maxMCPBodyBytes = 1 << 20

// httpHandler routes the MCP endpoint and the probe paths. Everything else
// is a 404, so a misconfigured client gets a clear answer instead of an MCP
// error.
func httpHandler(mcpServer *server.MCPServer) (http.Handler, *server.StreamableHTTPServer) {
	streamable := server.NewStreamableHTTPServer(mcpServer,
		server.WithEndpointPath(mcpEndpoint),
		// muster attaches the person's GitHub grant as a bearer on every
		// JSON-RPC request. Carrying it on the context is what makes a
		// served call the caller's own.
		server.WithHTTPContextFunc(gh.ContextWithBearer),
	)
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	mux.Handle(mcpEndpoint, http.MaxBytesHandler(gh.RequireBearer(streamable), maxMCPBodyBytes))
	return mux, streamable
}

// serveHTTP serves the MCP server over Streamable HTTP until ctx is done or
// SIGINT/SIGTERM arrives, then drains in-flight requests for shutdownGrace.
// No write timeout is set on purpose: a sweep tool call streams for as long
// as the sweep runs.
func serveHTTP(ctx context.Context, mcpServer *server.MCPServer, addr string, log io.Writer) error {
	handler, streamable := httpHandler(mcpServer)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		_, _ = fmt.Fprintf(log, "marge %s serving MCP over streamable HTTP on %s%s\n", version, addr, mcpEndpoint)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving on %s: %w", addr, err)
	case <-ctx.Done():
	}

	_, _ = fmt.Fprintln(log, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	streamable.CloseSessions(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

// scopeArguments are the arguments that say what a call covers. list and
// sweep share them, so the two scopes -- a team, or a query -- mean the same
// thing on every tool and on the CLI.
func scopeArguments() mcp.ToolOption {
	options := []mcp.ToolOption{
		mcp.WithString("team",
			mcp.Description("Cover this team's repositories (repositories/team-<name>.yaml in the team-file repository) under the team's policy."),
		),
		mcp.WithString("query",
			mcp.Description("Narrow the scope like `marge [query]`: GitHub search text (\"typescript\", \"repo:org/repo\"), or with repos/repos_file a filter on their org/repo."),
		),
		mcp.WithString("org",
			mcp.Description("GitHub organization or user to limit the scope to"),
		),
		mcp.WithString("repos_file",
			mcp.Description("Path to a file of org/repo lines (blank lines and # comments ignored) to scan instead of searching GitHub; merged with repos, duplicates dropped."),
		),
		mcp.WithArray("repos",
			mcp.Description("org/repo entries to scan instead of searching GitHub; merged with repos_file, duplicates dropped."),
			mcp.WithStringItems(),
		),
		mcp.WithArray("prs",
			mcp.Description("Only these PRs of the scope, each a URL or OWNER/REPO#NUMBER. The scope still picks repositories and policy, so a PR outside it is refused."),
			mcp.WithStringItems(),
		),
	}
	return func(tool *mcp.Tool) {
		for _, option := range options {
			option(tool)
		}
	}
}

// sweepTool declares the sweep tool and its arguments. parseSweepRequest
// reads exactly these arguments; the serve tests keep the two in step.
func sweepTool() mcp.Tool {
	return mcp.NewTool("sweep",
		tooldesc.Describe("Sweep a team's or query's bot PRs (Renovate, Align files, Herald, Dependabot): approve and merge eligible green ones.",
			"Commits changelog entries, labels each PR marge/<class>, comments evidence and applies the matching catalogue rule (may close a PR or rewrite required checks). "+
				"Majors and unreadable updates wait for a person; a pending or unreported required check is a wait, never a bypass; a failing security check is never merged past. "+
				"stale, cancelled, ci_no_verdict and obsolete are not action_required; rescue tooling acts on action_required only, skipping entries whose rescue is not stale."),
		scopeArguments(),
		mcp.WithArray("teams",
			mcp.Description("Several teams, each under its own team file and policy, swept together unless they share a repository; answers {\"teams\": [...]} in order."),
			mcp.WithStringItems(),
		),
		mcp.WithBoolean("merge_auto",
			mcp.Description("Also merge PRs that have auto-merge enabled (default: false)"),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Show what would be done without making changes (default: false). Stale PRs are still classified, but not refreshed."),
		),
		mcp.WithString("actions",
			mcp.Description("Comma-separated steps, in fixed order: classify, changelog, approve, merge, refresh, retry, remedy, mark (default all); remedy needs mark."),
		),
		mcp.WithString("security_patterns",
			mcp.Description("Comma-separated case-insensitive substrings added to the built-in list that flags failing CI checks as security-related"),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

// sweepRequest is what one call of the sweep tool asks for: the search
// inputs and the processing options, defaulting like the sweep CLI command.
// The repos_file argument lands in Opts.ReposFile, where the CLI flag of the
// same meaning lives.
type sweepRequest struct {
	Repos []string
	// Rule narrows the catalogue to one rule before matching. Empty runs
	// the whole catalogue, which is what a sweep does.
	Rule string
	Opts RunOptions
}

// resolveScope returns what the sweep covers: the repositories of the
// request merged with those of the scope, without duplicates, and the
// policy the scope resolves to. No repository at all means no restriction,
// so the PRs come from the GitHub search.
func (r sweepRequest) resolveScope(ctx context.Context, client *github.Client) (policy.Scope, error) {
	scope, err := r.Opts.resolveScope(ctx, client)
	if err != nil {
		return policy.Scope{}, err
	}
	scope.Repos = mergeRepos(r.Repos, scope.Repos)
	return scope, nil
}

// mergeRepos joins repository lists into one, trimmed and without
// duplicates, keeping the first spelling of each entry in order. GitHub
// treats owner and repository names case-insensitively, so the comparison
// does too. It returns nil when the lists hold no entry.
func mergeRepos(lists ...[]string) []string {
	seen := make(map[string]bool)
	var merged []string
	for _, list := range lists {
		for _, repo := range list {
			repo = strings.TrimSpace(repo)
			key := strings.ToLower(repo)
			if repo == "" || seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, repo)
		}
	}
	return merged
}

// parseSweepRequest reads the arguments declared by sweepTool. Quiet is
// always set: stdout is the MCP stdio transport, so no table, plain-text
// results or progress chatter may be written; the JSON result carries the
// same data.
func parseSweepRequest(request mcp.CallToolRequest) (sweepRequest, error) {
	actions, err := process.ParseActions(request.GetString("actions", ""))
	if err != nil {
		return sweepRequest{}, err
	}
	req := parseScope(request)
	req.Opts.DryRun = request.GetBool("dry_run", false)
	req.Opts.MergeAuto = request.GetBool("merge_auto", false)
	req.Opts.Actions = actions
	req.Opts.SecurityPatterns = request.GetString("security_patterns", "")
	return req, req.validate()
}

// parseScope reads the arguments scopeArguments declares. Quiet is always
// set: stdout is the MCP stdio transport, so no table, plain-text results or
// progress chatter may be written; the JSON result carries the same data.
func parseScope(request mcp.CallToolRequest) sweepRequest {
	return sweepRequest{
		Repos: request.GetStringSlice("repos", nil),
		Opts: RunOptions{
			Quiet:     true,
			NoTUI:     true,
			Query:     request.GetString("query", ""),
			Team:      request.GetString("team", ""),
			Org:       request.GetString("org", ""),
			ReposFile: request.GetString("repos_file", ""),
			PRs:       request.GetStringSlice("prs", nil),
		},
	}
}

// validate holds the same rule the sweep command enforces on its flags: the
// two scopes are exclusive, so a caller never believes a team's policy
// applied to a query it also passed.
func (r sweepRequest) validate() error {
	if r.Opts.Team != "" && (r.Opts.Query != "" || len(r.Repos) > 0 || r.Opts.Org != "" || r.Opts.ReposFile != "") {
		return errors.New("team is mutually exclusive with query, org, repos and repos_file")
	}
	return nil
}

// scopedPRs is what a tool starts from: the client it acts with, the login
// that client authenticates as, the PRs of the request's scope and the
// repositories the discovery could not list.
type scopedPRs struct {
	client *github.Client
	login  string
	prs    []pr.PRInfo
	// gone are the named PRs that merged or closed since the caller read
	// the list it named them from.
	gone  []gonePR
	found discovery
}

// discover resolves the request's scope and finds the PRs it covers. It
// records the resolved policies on the request, so the engine decides every
// PR under the policy the scope resolved to.
func (t toolset) discover(ctx context.Context, req *sweepRequest) (scopedPRs, error) {
	client, err := t.newClient(ctx)
	if err != nil {
		return scopedPRs{}, err
	}

	scope, err := req.resolveScope(ctx, client)
	if err != nil {
		return scopedPRs{}, err
	}
	req.Opts.Policies = scope.Policies

	login, err := gh.AuthenticatedLogin(ctx, client)
	if err != nil {
		return scopedPRs{}, err
	}

	repos, err := scopeRepos(scope.Repos, req.Opts.PRs)
	if err != nil {
		return scopedPRs{}, err
	}

	found, err := searchPRs(ctx, client, req.Opts.Query, login, repos, req.Opts.Parallel)
	if err != nil {
		return scopedPRs{}, fmt.Errorf("searching PRs: %w", err)
	}
	prs, gone, err := selectPRs(ctx, client, filterByOrg(found.PRs, req.Opts.Org), req.Opts.PRs, repos)
	if err != nil {
		return scopedPRs{}, err
	}

	return scopedPRs{client: client, login: login, prs: prs, gone: gone, found: found}, nil
}

// run is the one engine call behind list, sweep and remedy. It resolves the
// scope and its policy, searches the PRs, narrows them, loads the rule
// catalogue and processes them, exactly as the sweep command does. What a
// tool changes is the request it hands in, never the path it runs.
func (t toolset) run(ctx context.Context, req sweepRequest) (SweepResult, error) {
	scoped, err := t.discover(ctx, &req)
	if err != nil {
		return SweepResult{}, err
	}
	client, login, prs, found := scoped.client, scoped.login, scoped.prs, scoped.found

	catalogue, rulesReport := loadRules(ctx, client, RulesSource{}, req.Opts.Parallel)
	if req.Rule != "" {
		catalogue, err = catalogue.Only(req.Rule)
		if err != nil {
			return SweepResult{}, err
		}
		rulesReport.Loaded = len(catalogue.Rules)
	}
	req.Opts.Rules = catalogue

	status, err := processOnceWithStatus(ctx, client, login, prs, req.Opts)
	if err != nil {
		return SweepResult{}, fmt.Errorf("processing PRs: %w", err)
	}
	reportGone(status, scoped.gone)
	return buildSweepResult(status, found.Failed, rulesReport), nil
}

// toolError renders a call that failed.
func toolError(ctx context.Context, err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(errorText(ctx, err))
}

// errorText is what a caller reads of a failure. A rate refusal reads the
// same whichever request met it, GET /user or the hundredth check read, so
// the caller can tell it from other failures by its words and read the
// reset time off it.
func errorText(ctx context.Context, err error) string {
	if short, ok := errors.AsType[*gh.RateBudgetShort](err); ok {
		return short.Error()
	}
	if exhausted, ok := gh.AsRateLimit(ctx, err); ok {
		return exhausted.Error()
	}
	return err.Error()
}

// result renders a value as the tool's JSON payload.
func result(value any) (*mcp.CallToolResult, error) {
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshaling result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(jsonBytes)), nil
}

// listRequest is what one call of the list tool asks the engine for: the
// classify step alone, on a dry run, so the call writes nothing. The stored
// read runs no action and takes the scope from it alone.
func listRequest(request mcp.CallToolRequest) (sweepRequest, error) {
	req := parseScope(request)
	req.Opts.DryRun = true
	req.Opts.NoTUI = true
	req.Opts.Actions = process.ActionSet{process.ActionClassify: true}
	return req, req.validate()
}

func (t toolset) handleList(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	refresh := request.GetBool("refresh", false)
	if teams := request.GetStringSlice("teams", nil); len(teams) > 0 {
		if err := validateTeamsScope(request); err != nil {
			return toolError(ctx, err), nil
		}
		queues, err := t.listTeams(ctx, teams, refresh)
		if err != nil {
			return toolError(ctx, err), nil
		}
		return result(queues)
	}

	req, err := listRequest(request)
	if err != nil {
		return toolError(ctx, err), nil
	}
	run := t.listStored
	if refresh {
		run = t.run
	}
	sweepResult, err := run(ctx, req)
	if err != nil {
		return toolError(ctx, err), nil
	}
	return result(sweepResult)
}

// listStored reports the classification the last sweep stored on each PR
// instead of deciding it again. The classification lives in the PR's
// marge/<class> label, which the discovery already carries, so the whole
// list costs the discovery and nothing more: no check read, no PR read.
//
// What the label cannot say, the entry leaves out. Several states share one
// label, so the state is the class's representative; the evidence, the
// update type, the resolved policy and any prior rescue attempt each need
// the PR itself and stay empty. A PR no sweep has labelled has no stored
// classification, so it is reported as unclassified rather than guessed at.
func (t toolset) listStored(ctx context.Context, req sweepRequest) (SweepResult, error) {
	scoped, err := t.discover(ctx, &req)
	if err != nil {
		return SweepResult{}, err
	}
	status := storedStatus(scoped.prs)
	reportGone(status, scoped.gone)
	return buildSweepResult(status, scoped.found.Failed, nil), nil
}

// storedStatus reads each PR's stored classification out of its labels.
func storedStatus(prs []pr.PRInfo) *pr.PRStatus {
	status := pr.NewPRStatus()
	for _, info := range prs {
		idx := status.Add(info)
		status.SetClassification(idx, pr.DiscoveredKind(info), "")
		label, class := pr.StoredClass(info.Labels)
		state, classified := pr.ClassState(class)
		if !classified {
			status.Update(idx, pr.StatusUnclassified, "no sweep has classified this PR")
			continue
		}
		status.Update(idx, state, "stored by the last sweep")
		status.SetLabel(idx, label)
	}
	return status
}

func (t toolset) handleSweep(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, err := parseSweepRequest(request)
	if err != nil {
		return toolError(ctx, err), nil
	}
	if teams := request.GetStringSlice("teams", nil); len(teams) > 0 {
		if err := validateTeamsScope(request); err != nil {
			return toolError(ctx, err), nil
		}
		sweeps, err := t.sweepTeams(ctx, teams, req)
		if err != nil {
			return toolError(ctx, err), nil
		}
		return result(sweeps)
	}
	sweepResult, err := t.run(ctx, req)
	if err != nil {
		return toolError(ctx, err), nil
	}
	return result(sweepResult)
}

// remedyRequest is what one call of the remedy tool asks the engine for: the
// classify, remedy and mark steps on the one PR it names. mark comes with
// remedy because the once-per-change guard reads the evidence marker.
func remedyRequest(request mcp.CallToolRequest) (sweepRequest, error) {
	prRef := request.GetString("pr_url", "")
	owner, repo, _, err := pr.ParsePRRef(prRef)
	if err != nil {
		return sweepRequest{}, err
	}
	if owner == "" || repo == "" {
		return sweepRequest{}, fmt.Errorf("not a pull request reference: %s", prRef)
	}
	return sweepRequest{
		Rule: request.GetString("rule", ""),
		Opts: RunOptions{
			Quiet:   true,
			NoTUI:   true,
			DryRun:  request.GetBool("dry_run", false),
			Team:    request.GetString("team", ""),
			PRs:     []string{prRef},
			Actions: process.ActionSet{process.ActionClassify: true, process.ActionRemedy: true, process.ActionMark: true},
		},
	}, nil
}

func (t toolset) handleRemedy(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, err := remedyRequest(request)
	if err != nil {
		return toolError(ctx, err), nil
	}
	sweepResult, err := t.run(ctx, req)
	if err != nil {
		return toolError(ctx, err), nil
	}
	return result(sweepResult)
}

func (t toolset) handleMark(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	prRef := request.GetString("pr_url", "")
	outcome := request.GetString("outcome", "failed")
	reason := request.GetString("reason", "")
	tool := request.GetString("tool", "ai")
	dryRun := request.GetBool("dry_run", false)

	client, err := t.newClient(ctx)
	if err != nil {
		return toolError(ctx, err), nil
	}

	marker, owner, repo, number, err := markRescue(ctx, client, prRef, outcome, reason, tool, dryRun)
	if err != nil {
		return toolError(ctx, err), nil
	}

	written := map[string]any{
		"owner":    owner,
		"repo":     repo,
		"number":   number,
		"outcome":  marker.Outcome,
		"tool":     marker.Tool,
		"head_sha": marker.HeadSHA,
		"at":       marker.At.Format(time.RFC3339),
		"dry_run":  dryRun,
	}
	if marker.PatchID != "" {
		written["patch_id"] = marker.PatchID
	}
	if marker.ChangeID != "" {
		written["change_id"] = marker.ChangeID
	}
	return result(written)
}
