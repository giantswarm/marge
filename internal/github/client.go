// Package github creates the authenticated GitHub API client marge uses.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// NewClient returns a GitHub API client. It authenticates as the sweep App
// when the environment carries the App credential, and with the token
// LoadToken finds otherwise.
func NewClient(ctx context.Context) (*github.Client, error) {
	app, err := LoadApp()
	if err != nil {
		return nil, err
	}
	if app != nil {
		return NewAppClient(app, defaultBaseURL, nil)
	}
	token := LoadToken(ctx)
	if token == "" {
		return nil, errors.New("no GitHub token found: set GITHUB_TOKEN or GH_TOKEN, log in with `gh auth login`, or set the App credential (" + appIDEnv + ", " + appInstallationIDEnv + ", " + appPrivateKeyFileEnv + ")")
	}
	return github.NewClient(github.WithAuthToken(token))
}

// defaultBaseURL is the REST API root every call is built on. It carries the
// trailing slash go-github expects.
const defaultBaseURL = "https://api.github.com/"

// NewAppClient returns a client that authenticates as the App against
// baseURL. A nil httpClient uses the default transport. Only the tests pass
// either argument; NewClient supplies GitHub's own.
func NewAppClient(app *App, baseURL string, httpClient *http.Client) (*github.Client, error) {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport := &appTransport{
		app:     app,
		base:    base,
		baseURL: baseURL,
		client:  &http.Client{Transport: base, Timeout: httpClient.Timeout},
		tokens:  make(map[string]installationToken),
	}
	return github.NewClient(
		github.WithHTTPClient(&http.Client{Transport: transport, Timeout: httpClient.Timeout}),
		github.WithURLs(&baseURL, &baseURL),
	)
}

// AuthenticatedLogin returns the login the client acts as. An installation
// token has no user behind it, so GET /user answers 403 and the App's own
// slug names the bot instead.
func AuthenticatedLogin(ctx context.Context, client *github.Client) (string, error) {
	app, err := LoadApp()
	if err != nil {
		return "", err
	}
	if app == nil {
		user, _, err := client.Users.Get(ctx, "")
		if err != nil {
			return "", fmt.Errorf("getting authenticated user: %w", err)
		}
		return user.GetLogin(), nil
	}
	registered, _, err := client.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("getting the authenticated App: %w", err)
	}
	return registered.GetSlug() + "[bot]", nil
}
