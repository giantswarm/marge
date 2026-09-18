package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"
)

// OpenPR is one open pull request as a repository listing reports it.
type OpenPR struct {
	Owner     string
	Repo      string
	Number    int
	Title     string
	URL       string
	Author    string
	BaseRef   string
	CreatedAt time.Time
	Labels    []string
}

// RepoFailure names a repository whose pull requests could not be listed,
// and why. A repository that fails to list is reported, never silently
// dropped from a sweep.
type RepoFailure struct {
	Repo string
	Err  string
}

const (
	// reposPerQuery is how many repositories one GraphQL request covers.
	// Each one contributes its own page of pull requests to the node budget
	// GitHub bounds a query by, so the chunk stays well inside it.
	reposPerQuery = 25
	// queriesAtOnce is how many of those requests run together.
	queriesAtOnce = 8
	// prsPerPage is the page size of one repository's pull requests.
	prsPerPage = 100
	// labelsPerPR is how many labels are read per pull request. The stored
	// classification is one marge/<class> label among them.
	labelsPerPR = 25
)

// listedPR is one pull request of the GraphQL answer. An author that is a
// GitHub App answers with the App's slug, where REST answers with the slug
// and a [bot] suffix; login() restores the suffix, so a login means the same
// thing whichever API read it.
type listedPR struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	CreatedAt   time.Time `json:"createdAt"`
	BaseRefName string    `json:"baseRefName"`
	Author      *struct {
		Login    string `json:"login"`
		TypeName string `json:"__typename"`
	} `json:"author"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

func (p listedPR) login() string {
	if p.Author == nil {
		return ""
	}
	if p.Author.TypeName == "Bot" {
		return p.Author.Login + "[bot]"
	}
	return p.Author.Login
}

// listedRepo is one repository of the GraphQL answer. A repository the token
// cannot read answers null, and the error that says why carries its alias.
type listedRepo struct {
	PullRequests struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []listedPR `json:"nodes"`
	} `json:"pullRequests"`
}

// RepoRef is one repository to list, as owner and name.
type RepoRef struct {
	Owner string
	Name  string
}

func (r RepoRef) String() string { return r.Owner + "/" + r.Name }

// ParseRepoRefs reads "owner/name" entries. An entry that does not name both
// is dropped, exactly as the repository list of a team file is read today.
func ParseRepoRefs(repos []string) []RepoRef {
	refs := make([]RepoRef, 0, len(repos))
	for _, entry := range repos {
		owner, name, ok := strings.Cut(strings.TrimSpace(entry), "/")
		if !ok || owner == "" || name == "" {
			continue
		}
		refs = append(refs, RepoRef{Owner: owner, Name: name})
	}
	return refs
}

// ListOpenPRs returns the open pull requests of every repository named, and
// the repositories it could not read.
//
// One GraphQL request covers reposPerQuery repositories, so a scope of a few
// hundred repositories costs a handful of requests instead of one per
// repository. The order of the answer follows the order of refs, so the same
// scope lists the same way twice.
func ListOpenPRs(ctx context.Context, client *github.Client, refs []RepoRef) ([]OpenPR, []RepoFailure) {
	chunks := make([][]RepoRef, 0, len(refs)/reposPerQuery+1)
	for start := 0; start < len(refs); start += reposPerQuery {
		chunks = append(chunks, refs[start:min(start+reposPerQuery, len(refs))])
	}

	prs := make([][]OpenPR, len(chunks))
	failures := make([][]RepoFailure, len(chunks))

	var wg sync.WaitGroup
	slots := make(chan struct{}, queriesAtOnce)
	for index, chunk := range chunks {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			prs[index], failures[index] = listChunk(ctx, client, chunk)
		})
	}
	wg.Wait()

	var (
		allPRs      []OpenPR
		allFailures []RepoFailure
	)
	for index := range chunks {
		allPRs = append(allPRs, prs[index]...)
		allFailures = append(allFailures, failures[index]...)
	}
	sort.Slice(allFailures, func(i, j int) bool { return allFailures[i].Repo < allFailures[j].Repo })
	return allPRs, allFailures
}

// listChunk reads one request's worth of repositories.
func listChunk(ctx context.Context, client *github.Client, refs []RepoRef) ([]OpenPR, []RepoFailure) {
	query, variables := openPRsQuery(refs)
	answer := make(map[string]*listedRepo, len(refs))
	errs, err := graphQL(ctx, client, query, variables, &answer)
	if err != nil {
		failures := make([]RepoFailure, 0, len(refs))
		for _, ref := range refs {
			failures = append(failures, RepoFailure{Repo: ref.String(), Err: err.Error()})
		}
		return nil, failures
	}

	reasons := make(map[string]string, len(errs))
	var unattributed []string
	for _, entry := range errs {
		if alias := entry.alias(); alias != "" {
			reasons[alias] = entry.Message
			continue
		}
		unattributed = append(unattributed, entry.Message)
	}

	var (
		prs      []OpenPR
		failures []RepoFailure
	)
	for index, ref := range refs {
		alias := repoAlias(index)
		repo := answer[alias]
		if repo == nil {
			failures = append(failures, RepoFailure{Repo: ref.String(), Err: repoReason(reasons[alias], unattributed)})
			continue
		}
		page := repo.PullRequests
		prs = append(prs, openPRsOf(ref, page.Nodes)...)
		if !page.PageInfo.HasNextPage {
			continue
		}
		rest, failure := listRest(ctx, client, ref, page.PageInfo.EndCursor)
		prs = append(prs, rest...)
		if failure != nil {
			failures = append(failures, *failure)
		}
	}
	return prs, failures
}

// listRest reads the pull requests after cursor, one request a page. A
// repository with more than prsPerPage open pull requests is rare, so the
// pages are read for that repository alone rather than for its whole chunk.
func listRest(ctx context.Context, client *github.Client, ref RepoRef, cursor string) ([]OpenPR, *RepoFailure) {
	var prs []OpenPR
	for cursor != "" {
		var answer struct {
			Repository *listedRepo `json:"repository"`
		}
		_, err := graphQL(ctx, client, openPRsPageQuery, map[string]any{
			"owner": ref.Owner, "name": ref.Name, "after": cursor,
		}, &answer)
		if err != nil {
			return prs, &RepoFailure{Repo: ref.String(), Err: err.Error()}
		}
		if answer.Repository == nil {
			return prs, &RepoFailure{Repo: ref.String(), Err: "the repository answered nothing on the page after " + cursor}
		}
		page := answer.Repository.PullRequests
		prs = append(prs, openPRsOf(ref, page.Nodes)...)
		if !page.PageInfo.HasNextPage {
			break
		}
		cursor = page.PageInfo.EndCursor
	}
	return prs, nil
}

// repoReason says why a repository answered nothing. GitHub attributes most
// errors to the field they belong to; an error that names no field is
// reported against every repository of the chunk that answered nothing.
func repoReason(attributed string, unattributed []string) string {
	switch {
	case attributed != "":
		return attributed
	case len(unattributed) > 0:
		return strings.Join(unattributed, "; ")
	default:
		return "the repository answered nothing"
	}
}

func openPRsOf(ref RepoRef, nodes []listedPR) []OpenPR {
	prs := make([]OpenPR, 0, len(nodes))
	for _, node := range nodes {
		labels := make([]string, 0, len(node.Labels.Nodes))
		for _, label := range node.Labels.Nodes {
			labels = append(labels, label.Name)
		}
		if len(labels) == 0 {
			labels = nil
		}
		prs = append(prs, OpenPR{
			Owner:     ref.Owner,
			Repo:      ref.Name,
			Number:    node.Number,
			Title:     node.Title,
			URL:       node.URL,
			Author:    node.login(),
			BaseRef:   node.BaseRefName,
			CreatedAt: node.CreatedAt,
			Labels:    labels,
		})
	}
	return prs
}

func repoAlias(index int) string { return fmt.Sprintf("r%d", index) }

// prFields are the fields every listing reads. They are what the discovery
// carries into the sweep: the labels hold the classification a previous
// sweep stored, so a stored read needs nothing else.
const prFields = `
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        title
        url
        createdAt
        baseRefName
        author { login __typename }
        labels(first: %d) { nodes { name } }
      }`

// openPRsQuery builds the request for one chunk. The repository names travel
// as variables, so a name never becomes part of the query text.
func openPRsQuery(refs []RepoRef) (string, map[string]any) {
	variables := make(map[string]any, 2*len(refs))
	declarations := make([]string, 0, 2*len(refs))
	fields := make([]string, 0, len(refs))
	for index, ref := range refs {
		owner, name := fmt.Sprintf("o%d", index), fmt.Sprintf("n%d", index)
		variables[owner], variables[name] = ref.Owner, ref.Name
		declarations = append(declarations, fmt.Sprintf("$%s: String!, $%s: String!", owner, name))
		fields = append(fields, fmt.Sprintf(`  %s: repository(owner: $%s, name: $%s) {
    pullRequests(states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}, first: %d) {%s
    }
  }`, repoAlias(index), owner, name, prsPerPage, fmt.Sprintf(prFields, labelsPerPR)))
	}
	query := fmt.Sprintf("query(%s) {\n%s\n}", strings.Join(declarations, ", "), strings.Join(fields, "\n"))
	return query, variables
}

// openPRsPageQuery reads one further page of a single repository.
var openPRsPageQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $after: String!) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}, first: %d, after: $after) {%s
    }
  }
}`, prsPerPage, fmt.Sprintf(prFields, labelsPerPR))
