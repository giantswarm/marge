// Package github creates the authenticated GitHub API client marge uses.
package github

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"
)

// tokenEnvs are the environment variables checked for a token, in order.
// GITHUB_TOKEN is what marge has always read; GH_TOKEN is what the GitHub
// CLI reads, so a token exported for gh works for marge as well.
var tokenEnvs = []string{"GITHUB_TOKEN", "GH_TOKEN"}

// ghTimeout bounds the call to the GitHub CLI so a hanging gh cannot stall
// marge before it has done anything.
const ghTimeout = 5 * time.Second

// LoadToken returns the GitHub token from GITHUB_TOKEN, GH_TOKEN or, failing
// both, the GitHub CLI's own login (`gh auth token`). It returns "" when none
// of them yields a token.
func LoadToken(ctx context.Context) string {
	for _, env := range tokenEnvs {
		if tok := strings.TrimSpace(os.Getenv(env)); tok != "" {
			return tok
		}
	}
	return ghAuthToken(ctx)
}

// ghAuthToken asks the GitHub CLI for the token of its active login. A
// missing gh, a gh without a login or a slow gh all yield "".
func ghAuthToken(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// NewClient returns a GitHub API client authenticated with the token
// LoadToken finds.
func NewClient(ctx context.Context) (*github.Client, error) {
	token := LoadToken(ctx)
	if token == "" {
		return nil, errors.New("no GitHub token found: set GITHUB_TOKEN or GH_TOKEN, or log in with `gh auth login`")
	}
	return github.NewClient(github.WithAuthToken(token))
}
