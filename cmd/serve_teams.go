package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-github/v92/github"
	"github.com/mark3labs/mcp-go/mcp"

	gh "github.com/giantswarm/marge/internal/github"
	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/process"
	"github.com/giantswarm/marge/internal/rules"
)

// TeamQueue is one team's queue inside a list that covered several teams.
type TeamQueue struct {
	Team   string       `json:"team"`
	Result *SweepResult `json:"result,omitempty"`
	// Error says why the team has no result: it has no team file, or its
	// files do not parse. One team's failure leaves the others alone.
	Error string `json:"error,omitempty"`
}

// TeamQueues is what the list tool answers when it was given teams. The
// answer is an object and not a bare array, so a caller can tell a
// several-team answer from a single scope's SweepResult by its shape.
type TeamQueues struct {
	Teams []TeamQueue `json:"teams"`
}

// validateTeamsScope refuses a teams list that carries a second scope, the
// way the single-team scope is refused one.
func validateTeamsScope(request mcp.CallToolRequest) error {
	if request.GetString("team", "") != "" {
		return errors.New("teams is mutually exclusive with team: pass the one team under team, or every team under teams")
	}
	if request.GetString("query", "") != "" || request.GetString("org", "") != "" ||
		request.GetString("repos_file", "") != "" || len(request.GetStringSlice("repos", nil)) > 0 {
		return errors.New("teams is mutually exclusive with query, org, repos and repos_file")
	}
	return nil
}

// teamScope is one team's resolved scope, or the error that resolving it
// produced.
type teamScope struct {
	team  string
	scope policy.Scope
	err   error
}

// listTeams reads the queues of several teams in one call.
//
// The teams share the discovery: their repository lists are merged and read
// once, so a caller that wants every team pays one listing rather than one
// per team, and a repository two teams own is read once. Everything after
// the discovery stays per team, because a team file decides what its PRs
// may become: each team's PRs are classified under that team's own policy.
func (t toolset) listTeams(ctx context.Context, teams []string, refresh bool) (TeamQueues, error) {
	scoped, err := t.discoverTeams(ctx, teams)
	if err != nil {
		return TeamQueues{}, err
	}
	client, login, scopes, found := scoped.client, scoped.login, scoped.scopes, scoped.found

	queues := make([]TeamQueue, len(scopes))
	var wg sync.WaitGroup
	slots := make(chan struct{}, teamsAtOnce)
	for index, scoped := range scopes {
		if scoped.err != nil {
			queues[index] = TeamQueue{Team: scoped.team, Error: scoped.err.Error()}
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			queues[index] = t.teamQueue(ctx, client, login, scoped, found, refresh)
		})
	}
	wg.Wait()
	return TeamQueues{Teams: queues}, nil
}

// teamsScoped is the shared start of a call that covers several teams: the
// client, the login it authenticates as, each team's resolved scope and the
// one discovery their repositories share.
type teamsScoped struct {
	client *github.Client
	login  string
	scopes []teamScope
	found  discovery
}

// discoverTeams resolves every team's scope and reads their repositories
// once. A team whose files are missing or unreadable carries its error and
// leaves the others alone.
func (t toolset) discoverTeams(ctx context.Context, teams []string) (teamsScoped, error) {
	client, err := t.newClient(ctx)
	if err != nil {
		return teamsScoped{}, err
	}
	loader, err := policyLoader(client)
	if err != nil {
		return teamsScoped{}, err
	}
	login, err := gh.AuthenticatedLogin(ctx, client)
	if err != nil {
		return teamsScoped{}, err
	}

	scopes := resolveTeamScopes(ctx, loader, teams)

	repoLists := make([][]string, 0, len(scopes))
	for _, scoped := range scopes {
		if scoped.err == nil {
			repoLists = append(repoLists, scoped.scope.Repos)
		}
	}
	// No team resolved, so there is nothing to read. The discovery is not
	// run at all: without repositories it would fall back to the GitHub
	// search, and answer with PRs that belong to no team asked for.
	var found discovery
	if repos := mergeRepos(repoLists...); len(repos) > 0 {
		found, err = searchPRs(ctx, client, "", login, repos)
		if err != nil {
			return teamsScoped{}, fmt.Errorf("searching PRs: %w", err)
		}
	}

	return teamsScoped{client: client, login: login, scopes: scopes, found: found}, nil
}

// teamsAtOnce is how many teams of one call are classified together. It
// bounds the widest read, which is a refresh of every team: each team fans
// out over its own PRs as well.
const teamsAtOnce = 4

// teamQueue builds one team's queue from the shared discovery.
func (t toolset) teamQueue(ctx context.Context, client *github.Client, login string, scoped teamScope, found discovery, refresh bool) TeamQueue {
	prs := prsOfRepos(found.PRs, scoped.scope.Repos)
	failed := failuresOfRepos(found.Failed, scoped.scope.Repos)

	if !refresh {
		return TeamQueue{Team: scoped.team, Result: new(buildSweepResult(storedStatus(prs), failed, nil))}
	}

	opts := RunOptions{
		Quiet:    true,
		NoTUI:    true,
		DryRun:   true,
		Team:     scoped.team,
		Actions:  process.ActionSet{process.ActionClassify: true},
		Policies: scoped.scope.Policies,
	}
	status, err := processOnceWithStatus(ctx, client, login, prs, opts)
	if err != nil {
		return TeamQueue{Team: scoped.team, Error: err.Error()}
	}
	return TeamQueue{Team: scoped.team, Result: new(buildSweepResult(status, failed, nil))}
}

// resolveTeamScopes reads every team's files. The reads are independent, so
// they run together: one team's files are three requests, and a caller that
// asks for every team would otherwise wait for all of them in turn.
func resolveTeamScopes(ctx context.Context, loader policy.Loader, teams []string) []teamScope {
	scopes := make([]teamScope, len(teams))
	var wg sync.WaitGroup
	slots := make(chan struct{}, scopesAtOnce)
	for index, team := range teams {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			scope, err := loader.TeamScope(ctx, team)
			scopes[index] = teamScope{team: team, scope: scope, err: err}
		})
	}
	wg.Wait()
	return scopes
}

// scopesAtOnce is how many team files are read together.
const scopesAtOnce = 8

// prsOfRepos keeps the PRs that live in one of the repositories. GitHub
// treats owner and repository names case-insensitively, so the comparison
// does too.
func prsOfRepos(prs []pr.PRInfo, repos []string) []pr.PRInfo {
	wanted := repoSet(repos)
	kept := make([]pr.PRInfo, 0, len(prs))
	for _, info := range prs {
		if wanted[strings.ToLower(info.Owner+"/"+info.Repo)] {
			kept = append(kept, info)
		}
	}
	return kept
}

// failuresOfRepos keeps the failures of one of the repositories, so a team
// is told about its own unreadable repositories and not about another
// team's.
func failuresOfRepos(failures []repoFailure, repos []string) []repoFailure {
	wanted := repoSet(repos)
	kept := make([]repoFailure, 0, len(failures))
	for _, failure := range failures {
		if wanted[strings.ToLower(failure.Repo)] {
			kept = append(kept, failure)
		}
	}
	return kept
}

func repoSet(repos []string) map[string]bool {
	set := make(map[string]bool, len(repos))
	for _, repo := range repos {
		set[strings.ToLower(strings.TrimSpace(repo))] = true
	}
	return set
}

// sweepTeams sweeps several teams in one call, each under its own policy.
//
// The teams share the discovery, as a several-team list does. The sweeps
// themselves run one after another: a sweep writes, and two teams can own
// the same repository, so running them together would let two runs act on
// one PR at the same time.
func (t toolset) sweepTeams(ctx context.Context, teams []string, req sweepRequest) (TeamQueues, error) {
	scoped, err := t.discoverTeams(ctx, teams)
	if err != nil {
		return TeamQueues{}, err
	}

	selected, err := prsByTeam(scoped.scopes, req.Opts.PRs)
	if err != nil {
		return TeamQueues{}, err
	}

	catalogue, rulesReport := loadRules(ctx, scoped.client, RulesSource{})
	if req.Rule != "" {
		catalogue, err = catalogue.Only(req.Rule)
		if err != nil {
			return TeamQueues{}, err
		}
		rulesReport.Loaded = len(catalogue.Rules)
	}

	queues := make([]TeamQueue, len(scoped.scopes))
	for index, scope := range scoped.scopes {
		if scope.err != nil {
			queues[index] = TeamQueue{Team: scope.team, Error: scope.err.Error()}
			continue
		}
		if err := ctx.Err(); err != nil {
			return TeamQueues{}, err
		}
		queues[index] = t.sweepTeam(ctx, scoped, scope, req, selected[scope.team], catalogue, rulesReport)
	}
	return TeamQueues{Teams: queues}, nil
}

// sweepTeam sweeps one team of a several-team call from the shared
// discovery.
func (t toolset) sweepTeam(ctx context.Context, scoped teamsScoped, scope teamScope, req sweepRequest, prs []string, catalogue *rules.Catalogue, rulesReport *SweepRules) TeamQueue {
	found := prsOfRepos(scoped.found.PRs, scope.scope.Repos)
	failed := failuresOfRepos(scoped.found.Failed, scope.scope.Repos)
	if len(req.Opts.PRs) > 0 {
		var err error
		if found, err = filterByPRs(found, prs); err != nil {
			return TeamQueue{Team: scope.team, Error: err.Error()}
		}
	}

	opts := req.Opts
	opts.Team = scope.team
	opts.PRs = prs
	opts.Policies = scope.scope.Policies
	opts.Rules = catalogue

	status, err := processOnceWithStatus(ctx, scoped.client, scoped.login, found, opts)
	if err != nil {
		return TeamQueue{Team: scope.team, Error: err.Error()}
	}
	return TeamQueue{Team: scope.team, Result: new(buildSweepResult(status, failed, rulesReport))}
}

// prsByTeam assigns each named PR to the team that owns its repository. A
// PR no team of the call owns is refused for the whole call, the way a PR
// outside a single team's scope is: a caller never believes a PR was swept
// because it was silently absent.
func prsByTeam(scopes []teamScope, refs []string) (map[string][]string, error) {
	byTeam := make(map[string][]string, len(scopes))
	if len(refs) == 0 {
		return byTeam, nil
	}
	owners := make(map[string]string, len(scopes))
	for _, scope := range scopes {
		if scope.err != nil {
			continue
		}
		for _, repo := range scope.scope.Repos {
			if _, taken := owners[strings.ToLower(repo)]; !taken {
				owners[strings.ToLower(repo)] = scope.team
			}
		}
	}

	var orphans []string
	for _, ref := range refs {
		owner, repo, _, err := pr.ParsePRRef(ref)
		if err != nil {
			return nil, err
		}
		team, owned := owners[strings.ToLower(owner+"/"+repo)]
		if !owned {
			orphans = append(orphans, ref)
			continue
		}
		byTeam[team] = append(byTeam[team], ref)
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return nil, fmt.Errorf("no team of this call owns the repository of: %s", strings.Join(orphans, ", "))
	}
	return byTeam, nil
}
