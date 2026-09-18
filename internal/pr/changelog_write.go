package pr

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v92/github"
)

// ChangelogOutcome is what one attempt to write an entry produced. Exactly
// one of Written and Refused is set, and Line carries the entry either way,
// so a dry run reports what it would have written.
type ChangelogOutcome struct {
	Line    string
	Written bool
	Commit  string
	// Refused says why nothing was written, and is not a failure: the
	// repository keeps no changelog, or the entry is already in it.
	Refused string
}

// WriteChangelogEntry adds the team's entry to a PR's branch, and reports
// what it did.
//
// The entry is written once: a file that already carries the line is left
// untouched, whichever call put it there and however many times it is asked
// for. The check is on the line itself, so it survives a rebase, a second
// sweep and a person who wrote the entry by hand.
//
// dryRun renders the line and reads the file, and writes nothing.
func WriteChangelogEntry(
	ctx context.Context,
	client *github.Client,
	policy ChangelogPolicy,
	facts ChangelogFacts,
	owner, repo, branch string,
	dryRun bool,
) (ChangelogOutcome, error) {
	line, err := ChangelogLine(policy, facts)
	if err != nil {
		return ChangelogOutcome{}, err
	}
	outcome := ChangelogOutcome{Line: line}

	content, sha, found, err := changelogFile(ctx, client, owner, repo, policy.Path, branch)
	if err != nil {
		return outcome, err
	}
	if !found {
		outcome.Refused = owner + "/" + repo + " has no " + policy.Path
		return outcome, nil
	}

	next, changed := InsertChangelogLine(content, policy, line)
	if !changed {
		outcome.Refused = "the entry is already in " + policy.Path
		return outcome, nil
	}
	if dryRun {
		return outcome, nil
	}

	commit, _, err := client.Repositories.UpdateFile(ctx, owner, repo, policy.Path, &github.RepositoryContentFileOptions{
		Message: new("docs(changelog): record " + facts.PR),
		Content: []byte(next),
		SHA:     new(sha),
		Branch:  new(branch),
	})
	if err != nil {
		return outcome, fmt.Errorf("committing the entry: %w", err)
	}
	outcome.Written = true
	outcome.Commit = commit.GetSHA()
	return outcome, nil
}

// changelogFile reads the file on the branch and returns its content and blob
// SHA. A repository that keeps no changelog reports found false and no error:
// plenty of repositories have none, and where a changelog goes is the
// repository's own convention, which this does not invent.
func changelogFile(ctx context.Context, client *github.Client, owner, repo, path, branch string) (content, sha string, found bool, err error) {
	file, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	if file == nil {
		return "", "", false, errors.New(path + " is a directory, not a changelog file")
	}
	content, err = file.GetContent()
	if err != nil {
		return "", "", false, fmt.Errorf("decoding %s: %w", path, err)
	}
	return content, file.GetSHA(), true, nil
}
