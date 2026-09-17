package policy

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/go-github/v92/github"
	"gopkg.in/yaml.v3"
)

// DefaultFile holds the company defaults; TeamFile is one team's
// deviations and RepositoriesFile is the team's repository list, which also
// carries the per-repository exceptions.
const DefaultFile = "bot-prs-sweep/default.yaml"

// TeamFilePrefix and teamFileSuffix bracket a team name in the path of its
// policy file, and are what Teams reads a team name back out of.
const TeamFilePrefix = "bot-prs-sweep/team-"

const teamFileSuffix = ".yaml"

// PolicyDir holds the company defaults and every team's policy file.
const PolicyDir = "bot-prs-sweep"

// TeamFile returns the path of a team's policy file.
func TeamFile(team string) string {
	return TeamFilePrefix + team + teamFileSuffix
}

// RepositoriesFile returns the path of a team's repository list.
func RepositoriesFile(team string) string {
	return fmt.Sprintf("repositories/team-%s.yaml", team)
}

// teamName is the shape of a team name. The name reaches the loader from a
// flag and from an MCP request, and it becomes part of two file paths, so
// only the shape a team slug actually has is accepted.
var teamName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validateTeam refuses a team name that cannot name a policy file.
func validateTeam(team string) error {
	if !teamName.MatchString(team) {
		return fmt.Errorf("team %q is not a team name: lowercase letters, digits and dashes, starting with a letter or a digit", team)
	}
	return nil
}

// Source holds the policy files. Read returns the content of path, and
// reports found false for a path the source does not hold, which is not an
// error: an absent policy file falls back to the company defaults. A Source
// names where it looked in its String, so an error can say so.
//
// GitHubSource is the only source the sweep ships. Nothing else in the
// package knows where a file was read from.
type Source interface {
	fmt.Stringer
	Read(ctx context.Context, path string) (content string, found bool, err error)
	// List returns the paths of the files directly under dir, and reports
	// found false for a directory the source does not hold.
	List(ctx context.Context, dir string) (paths []string, found bool, err error)
}

// Loader resolves a sweep's scope and policy from the files of one Source.
type Loader struct {
	Source Source
	// Owner is the GitHub organisation the repository lists name their
	// entries under. It is a property of what the files say, not of where
	// they are read from, so it stays on the loader.
	Owner string
}

// GitHubSource reads the files from the default branch of one repository,
// giantswarm/github in every real run.
type GitHubSource struct {
	Client *github.Client
	Owner  string
	Repo   string
}

func (g GitHubSource) String() string { return g.Owner + "/" + g.Repo }

// Read returns the content of path on the repository's default branch.
// GitHub answers 404 both for a file that is not there and for a repository
// the token cannot read, so neither is reported as an error here.
func (g GitHubSource) Read(ctx context.Context, path string) (string, bool, error) {
	file, _, resp, err := g.Client.Repositories.GetContents(ctx, g.Owner, g.Repo, path, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	content, err := file.GetContent()
	if err != nil {
		return "", false, fmt.Errorf("decoding %s: %w", path, err)
	}
	return content, true, nil
}

// List returns the paths of the files in dir on the repository's default
// branch. A directory that is not there, like a repository the token cannot
// read, answers 404 and is reported as not found.
func (g GitHubSource) List(ctx context.Context, dir string) ([]string, bool, error) {
	_, entries, resp, err := g.Client.Repositories.GetContents(ctx, g.Owner, g.Repo, dir, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("listing %s: %w", dir, err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.GetType() != "file" {
			continue
		}
		paths = append(paths, entry.GetPath())
	}
	return paths, true, nil
}

// Teams returns every team that has a policy file, sorted. A team opts its
// scheduled sweep in by writing that file, so this is the list the schedule
// starts from; the schedule key of each file then decides which of them run.
func (l Loader) Teams(ctx context.Context) ([]string, error) {
	paths, found, err := l.Source.List(ctx, PolicyDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no policy directory: %s has no %s, or it cannot be read", l.Source, PolicyDir)
	}
	var teams []string
	for _, path := range paths {
		name, ok := strings.CutPrefix(path, TeamFilePrefix)
		if !ok {
			continue
		}
		name, ok = strings.CutSuffix(name, teamFileSuffix)
		if !ok || validateTeam(name) != nil {
			continue
		}
		teams = append(teams, name)
	}
	slices.Sort(teams)
	return teams, nil
}

// Scope is what one sweep resolves before it starts: the repositories it
// covers and the policy they are swept under. Repos is nil outside the team
// scope, where the PRs come from a GitHub search instead.
type Scope struct {
	Repos    []string
	Policies *Set
}

// TeamScope reads the team's repository list, the company defaults and the
// team's policy file, and resolves all three. A missing repository list is
// an error: without it the sweep has no scope. A missing policy file is
// not: the company defaults then apply on their own, and pr.Policy.Sources
// names the files that were read.
//
// The repository list is read first. GitHub answers 404 for a repository
// the token cannot read, so an absent policy file and an unreadable
// giantswarm/github look the same. Reading the one file that must exist
// first turns that case into one error that names the access.
func (l Loader) TeamScope(ctx context.Context, team string) (Scope, error) {
	if err := validateTeam(team); err != nil {
		return Scope{}, err
	}
	path := RepositoriesFile(team)
	content, found, err := l.Source.Read(ctx, path)
	if err != nil {
		return Scope{}, err
	}
	if !found {
		return Scope{}, fmt.Errorf("no team file for %q: %s has no %s, or it cannot be read", team, l.Source, path)
	}
	repos, exceptions, err := ParseRepositories(content, l.Owner, path)
	if err != nil {
		return Scope{}, err
	}

	files, err := l.companyAndTeamFiles(ctx, team)
	if err != nil {
		return Scope{}, err
	}

	set, err := NewSet(files, exceptions)
	if err != nil {
		return Scope{}, fmt.Errorf("%s: %w", path, err)
	}
	return Scope{Repos: repos, Policies: set}, nil
}

// QueryScope reads the company defaults alone. The query scope has no team,
// so no team file and no repository exception apply to it.
func (l Loader) QueryScope(ctx context.Context) (Scope, error) {
	doc, err := l.document(ctx, DefaultFile)
	if err != nil {
		return Scope{}, err
	}
	set, err := NewSet([]File{{Path: DefaultFile, Doc: doc}}, nil)
	if err != nil {
		return Scope{}, err
	}
	return Scope{Policies: set}, nil
}

func (l Loader) companyAndTeamFiles(ctx context.Context, team string) ([]File, error) {
	defaults, err := l.document(ctx, DefaultFile)
	if err != nil {
		return nil, err
	}
	teamPath := TeamFile(team)
	teamDoc, err := l.document(ctx, teamPath)
	if err != nil {
		return nil, err
	}
	return []File{{Path: DefaultFile, Doc: defaults}, {Path: teamPath, Doc: teamDoc}}, nil
}

// document reads and parses one policy file. A file that is not there
// yields a nil document; every other read problem is an error.
func (l Loader) document(ctx context.Context, path string) (*Document, error) {
	content, found, err := l.Source.Read(ctx, path)
	if err != nil || !found {
		return nil, err
	}
	return ParseDocument(path, content)
}

// ParseRepositories returns the owner/name entries of a team's repository
// list together with the botPRsSweep exception of every entry that carries
// one, keyed by the same owner/name. Only the name and that one key are
// read; every other key of the file belongs to the generators and changes
// without notice. The repositories live under the list's own owner.
func ParseRepositories(content, owner, path string) ([]string, map[string]Exception, error) {
	var entries []struct {
		Name string `yaml:"name"`
		// A yaml.Node by value: the decoder leaves a pointer field empty.
		// Kind is zero on an entry that has no exception.
		BotPRsSweep yaml.Node `yaml:"botPRsSweep"`
	}
	if err := yaml.Unmarshal([]byte(content), &entries); err != nil {
		return nil, nil, fmt.Errorf("parsing team file %s: %w", path, err)
	}
	var repos []string
	exceptions := make(map[string]Exception)
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		repo := owner + "/" + name
		repos = append(repos, repo)
		if entry.BotPRsSweep.Kind == 0 {
			continue
		}
		var exception Exception
		if err := strictDecodeNode(&entry.BotPRsSweep, &exception); err != nil {
			return nil, nil, fmt.Errorf("parsing botPRsSweep of repository %s in %s: %w", repo, path, err)
		}
		// Exception.apply resolves the names again, for a caller that
		// builds a Set without this function. Here the file path is still
		// in hand, so the error names the file.
		if _, err := exception.resolve(repo); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		exceptions[repo] = exception
	}
	if len(repos) == 0 {
		return nil, nil, fmt.Errorf("team file %s lists no repositories", path)
	}
	return repos, exceptions, nil
}
