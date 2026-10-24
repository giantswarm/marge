package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/google/go-github/v93/github"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/marge/internal/policy"
	"github.com/giantswarm/marge/internal/pr"
)

// changelogTool declares the changelog tool. The sweep writes the entry on
// the PRs it is about to approve; this door writes it on the PRs a person
// picked, whatever the team's policy says about the sweep. Both refuse a
// repository whose release notes are generated from its commits.
func changelogTool() mcp.Tool {
	return mcp.NewTool("changelog",
		tooldesc.Describe("Add one changelog entry, in the team's own format, to bot PRs and commit it to each PR's own branch.",
			"The format is the changelog section of the team's bot-prs-sweep/team-<name>.yaml (file, heading, section and a line template over dependency, versions, PR, repository, bot and update type), else the company default. "+
				"Nothing else is written: no approval, merge, label or comment. A PR already carrying the line is left as is. "+
				"A PR on a fork branch, by an untrusted author, or in a repository without the file or with a cliff.toml (notes from commits) is refused with the reason."),
		mcp.WithArray("prs",
			mcp.Required(),
			mcp.Description("The pull requests to write an entry on, each a PR URL or OWNER/REPO#NUMBER."),
			mcp.WithStringItems(),
		),
		mcp.WithString("team",
			mcp.Description("Use this team's format, from bot-prs-sweep/team-<name>.yaml in the team-file repository; without it, the company default format."),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("Return the line each PR would receive and write nothing (default: false)"),
		),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

// ChangelogEntry is what one PR of a changelog call produced.
type ChangelogEntry struct {
	PR   string `json:"pr"`
	Path string `json:"path,omitempty"`
	// Line is the entry the team's format produced, whether or not it was
	// written.
	Line string `json:"line,omitempty"`
	// Written is true when the commit was made. A dry run, a PR that
	// already carries the line and a refusal all leave it false.
	Written bool `json:"written"`
	// Refused carries the reason nothing was written, empty when the call
	// wrote or would have written.
	Refused string `json:"refused,omitempty"`
	// Commit is the SHA of the commit that added the entry.
	Commit string `json:"commit,omitempty"`
}

// ChangelogResult is the answer of one changelog call.
type ChangelogResult struct {
	DryRun  bool             `json:"dry_run"`
	Written int              `json:"written"`
	Refused int              `json:"refused"`
	Entries []ChangelogEntry `json:"entries"`
}

func (t toolset) handleChangelog(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	refs := request.GetStringSlice("prs", nil)
	if len(refs) == 0 {
		return mcp.NewToolResultError("prs: name at least one pull request"), nil
	}
	dryRun := request.GetBool("dry_run", false)

	client, err := t.newClient(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	policies, err := changelogPolicies(ctx, client, request.GetString("team", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	answer := ChangelogResult{DryRun: dryRun}
	for _, ref := range refs {
		entry := changelogEntry(ctx, client, policies, ref, dryRun)
		switch {
		case entry.Written:
			answer.Written++
		case entry.Refused != "":
			answer.Refused++
		}
		answer.Entries = append(answer.Entries, entry)
	}
	return result(answer)
}

// changelogPolicies resolves the policy set the entries are written under:
// the team's when one is named, the company defaults otherwise.
func changelogPolicies(ctx context.Context, client *github.Client, team string) (*policy.Set, error) {
	if team == "" {
		return nil, nil
	}
	loader, err := policyLoader(client)
	if err != nil {
		return nil, err
	}
	scope, err := loader.TeamScope(ctx, team)
	if err != nil {
		return nil, err
	}
	return scope.Policies, nil
}

// changelogEntry writes one PR's entry, or says why it did not.
func changelogEntry(ctx context.Context, client *github.Client, policies *policy.Set, ref string, dryRun bool) ChangelogEntry {
	entry := ChangelogEntry{PR: ref}
	owner, repo, number, err := pr.ParsePRRef(ref)
	if err != nil {
		entry.Refused = err.Error()
		return entry
	}
	entry.PR = fmt.Sprintf("%s/%s#%d", owner, repo, number)

	pull, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		entry.Refused = "reading the pull request: " + err.Error()
		return entry
	}

	kind := pr.KindOfPull(owner, pull)
	if kind == "" {
		entry.Refused = fmt.Sprintf("%s authored this PR, and only a trusted bot's PR earns an entry: %s", pull.GetUser().GetLogin(), strings.Join(pr.TrustedAuthors(), ", "))
		return entry
	}
	head := pull.GetHead()
	if !strings.EqualFold(head.GetRepo().GetFullName(), owner+"/"+repo) {
		entry.Refused = "the branch lives in a fork, which this call does not write to"
		return entry
	}
	if pull.GetState() != "open" {
		entry.Refused = "the pull request is " + pull.GetState()
		return entry
	}

	resolved := policies.For(owner, repo).Changelog
	entry.Path = resolved.Path
	from, to := pr.ExtractVersions(pull.GetTitle())

	outcome, err := pr.WriteChangelogEntry(ctx, client, resolved, pr.ChangelogFacts{
		Dependency: pr.ExtractDependencyName(pull.GetTitle()),
		From:       from,
		To:         to,
		PR:         entry.PR,
		Repository: owner + "/" + repo,
		Kind:       string(kind),
		UpdateType: string(pr.ClassifyUpdate(kind, pull.GetTitle(), pull.GetBody())),
	}, owner, repo, head.GetRef(), dryRun)
	entry.Line = outcome.Line
	switch {
	case err != nil:
		entry.Refused = err.Error()
	default:
		entry.Refused = outcome.Refused
		entry.Written = outcome.Written
		entry.Commit = outcome.Commit
	}
	return entry
}
