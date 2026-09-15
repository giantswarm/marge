package policy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v92/github"
	"gopkg.in/yaml.v3"
)

// DefaultFile holds the company defaults; TeamFile is one team's
// deviations and RepositoriesFile is the team's repository list, which also
// carries the per-repository exceptions.
const DefaultFile = "bot-prs-sweep/default.yaml"

// TeamFile returns the path of a team's policy file.
func TeamFile(team string) string {
	return fmt.Sprintf("bot-prs-sweep/team-%s.yaml", team)
}

// RepositoriesFile returns the path of a team's repository list.
func RepositoriesFile(team string) string {
	return fmt.Sprintf("repositories/team-%s.yaml", team)
}

// Loader reads the policy files from the default branch of one repository,
// giantswarm/github in every real run.
type Loader struct {
	Client *github.Client
	Owner  string
	Repo   string
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
	path := RepositoriesFile(team)
	content, found, err := l.read(ctx, path)
	if err != nil {
		return Scope{}, err
	}
	if !found {
		return Scope{}, fmt.Errorf("no team file for %q: %s/%s has no %s, or the token cannot read %s/%s", team, l.Owner, l.Repo, path, l.Owner, l.Repo)
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
	// A team file is the team's opt-in to the schedule. The schedule key
	// exists only to pause the schedule again without deleting the file.
	if teamDoc != nil && teamDoc.Schedule == nil {
		teamDoc = withSchedule(teamDoc, scheduleEnabled)
	}
	return []File{{Path: DefaultFile, Doc: defaults}, {Path: teamPath, Doc: teamDoc}}, nil
}

// withSchedule returns a copy of doc whose schedule key is set.
func withSchedule(doc *Document, value string) *Document {
	out := *doc
	out.Schedule = &value
	return &out
}

// document reads and parses one policy file. A file that is not there
// yields a nil document; every other read problem is an error.
func (l Loader) document(ctx context.Context, path string) (*Document, error) {
	content, found, err := l.read(ctx, path)
	if err != nil || !found {
		return nil, err
	}
	return ParseDocument(path, content)
}

// read returns the content of path on the repository's default branch.
func (l Loader) read(ctx context.Context, path string) (content string, found bool, err error) {
	file, _, resp, err := l.Client.Repositories.GetContents(ctx, l.Owner, l.Repo, path, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	content, err = file.GetContent()
	if err != nil {
		return "", false, fmt.Errorf("decoding %s: %w", path, err)
	}
	return content, true, nil
}

// ParseRepositories returns the owner/name entries of a team's repository
// list together with the botPRsSweep exception of every entry that carries
// one. Only the name and that one key are read; every other key of the file
// belongs to the generators and changes without notice. The repositories
// live under the list's own owner.
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
		repos = append(repos, owner+"/"+name)
		if entry.BotPRsSweep.Kind == 0 {
			continue
		}
		var exception Exception
		if err := strictDecodeNode(&entry.BotPRsSweep, &exception); err != nil {
			return nil, nil, fmt.Errorf("parsing botPRsSweep of repository %s in %s: %w", name, path, err)
		}
		// Exception.apply validates again, for a caller that builds a Set
		// without this function. Here the file path is still in hand.
		if err := exception.validate(name); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		exceptions[name] = exception
	}
	if len(repos) == 0 {
		return nil, nil, fmt.Errorf("team file %s lists no repositories", path)
	}
	return repos, exceptions, nil
}
