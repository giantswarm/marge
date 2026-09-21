package pr

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v92/github"
)

// cliffConfigPath is git-cliff's configuration, and the mark of a repository
// whose release notes are generated from its commits.
const cliffConfigPath = "cliff.toml"

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
// The entry belongs to a repository that cuts its release from the changelog
// file. A repository whose release notes are generated from its commits
// publishes the update already, so it is refused; see ReleaseNotesGenerated.
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

	// On the default branch, not the PR's: how a repository cuts its release
	// is the repository's own setting, and a bot branch never changes it.
	generated, err := ReleaseNotesGenerated(ctx, client, owner, repo, "")
	if err != nil {
		return outcome, err
	}
	if generated {
		outcome.Refused = owner + "/" + repo + " generates its release notes with git-cliff"
		return outcome, nil
	}

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

// ReleaseNotesGenerated reports whether the repository builds its release
// notes from its commits rather than from a changelog file, which it does when
// it carries a cliff.toml.
//
// git-cliff groups a bot's commit like any other -- the Giant Swarm
// configuration maps chore and fix to Changed and Fixed -- so the update is
// already published on the release page, and a line added to the file would
// only repeat it. Such a repository cuts no version section either, so the
// line would sit under an unreleased heading that nothing releases.
//
// A repository without the file reports false and no error, as every other
// read here does. An empty ref reads the default branch.
func ReleaseNotesGenerated(ctx context.Context, client *github.Client, owner, repo, ref string) (bool, error) {
	_, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, cliffConfigPath, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", cliffConfigPath, err)
	}
	return true, nil
}
