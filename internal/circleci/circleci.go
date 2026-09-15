// Package circleci is a minimal client for the CircleCI API. marge uses it
// to look behind a failing "ci/circleci: <job>" commit status and tell a
// build that really failed apart from one CircleCI cancelled itself, and to
// rerun the latter so the same commit gets a real verdict.
//
// Builds are read through the v1.1 API; reruns go through the v2 workflow
// rerun endpoint, which also releases the jobs the cancel left blocked.
//
// Only the handful of fields marge needs are modelled. The v1.1 build JSON
// is public for public projects; private projects and every rerun endpoint
// need an API token, sent as the Circle-Token header (never as basic auth or
// a query parameter).
package circleci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the CircleCI API host.
const DefaultBaseURL = "https://circleci.com"

// tokenEnv is the environment variable the CircleCI CLI itself honours for
// its API token; marge reads the same one so one export serves both.
const tokenEnv = "CIRCLECI_CLI_TOKEN"

// maxBodyBytes caps how much of a build response is read. A v1.1 build with
// full step output is a few hundred kilobytes; this leaves ample room while
// keeping a misbehaving server from exhausting memory.
const maxBodyBytes = 16 << 20

// BuildRef identifies one CircleCI job -- a "build" in v1.1 terms -- by the
// path components the API uses.
type BuildRef struct {
	// VCS is the API path segment for the VCS provider: "github" or
	// "bitbucket".
	VCS   string
	Owner string
	Repo  string
	Num   int
}

// vcsSegments maps the provider aliases found in CircleCI URLs to the
// segment the v1.1 API expects.
var vcsSegments = map[string]string{
	"gh":        "github",
	"github":    "github",
	"bb":        "bitbucket",
	"bitbucket": "bitbucket",
}

// ParseBuildURL extracts the build reference from the target_url CircleCI
// attaches to its commit statuses. Two shapes are recognised:
//
//	https://circleci.com/gh/<owner>/<repo>/<build_num>
//	https://app.circleci.com/pipelines/github/<owner>/<repo>/<pipeline>/workflows/<id>/jobs/<build_num>
//
// Anything else -- another CI system, a CircleCI URL without a build number
// -- returns ok == false.
func ParseBuildURL(raw string) (ref BuildRef, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return BuildRef{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host != "circleci.com" && !strings.HasSuffix(host, ".circleci.com") {
		return BuildRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	// app.circleci.com: pipelines/<vcs>/<owner>/<repo>/<pipeline>/workflows/<id>/jobs/<num>
	if len(parts) >= 9 && parts[0] == "pipelines" && parts[len(parts)-2] == "jobs" {
		parts = []string{parts[1], parts[2], parts[3], parts[len(parts)-1]}
	}
	if len(parts) != 4 {
		return BuildRef{}, false
	}
	vcs, known := vcsSegments[strings.ToLower(parts[0])]
	if !known || parts[1] == "" || parts[2] == "" {
		return BuildRef{}, false
	}
	num, err := strconv.Atoi(parts[3])
	if err != nil || num <= 0 {
		return BuildRef{}, false
	}
	return BuildRef{VCS: vcs, Owner: parts[1], Repo: parts[2], Num: num}, true
}

// path is the v1.1 API path of the build, without the host.
func (r BuildRef) path() string {
	return fmt.Sprintf("/api/v1.1/project/%s/%s/%s/%d",
		url.PathEscape(r.VCS), url.PathEscape(r.Owner), url.PathEscape(r.Repo), r.Num)
}

// Build is the subset of a v1.1 build that marge inspects.
type Build struct {
	BuildNum    int    `json:"build_num"`
	BuildURL    string `json:"build_url"`
	Branch      string `json:"branch"`
	Status      string `json:"status"`
	Outcome     string `json:"outcome"`
	Lifecycle   string `json:"lifecycle"`
	VCSRevision string `json:"vcs_revision"`
	Canceled    bool   `json:"canceled"`
	Steps       []Step `json:"steps"`
	Workflows   struct {
		JobName string `json:"job_name"`
		// WorkflowID identifies the workflow run the build belongs to. It
		// is the handle the v2 rerun endpoint takes; a build outside a
		// workflow carries none.
		WorkflowID   string `json:"workflow_id"`
		WorkflowName string `json:"workflow_name"`
	} `json:"workflows"`
}

// Step is one named step of a build; a step has one action per parallel
// container.
type Step struct {
	Name    string   `json:"name"`
	Actions []Action `json:"actions"`
}

// Action is the outcome of one step on one container.
type Action struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Canceled bool   `json:"canceled"`
	Failed   bool   `json:"failed"`
}

// AutoCancelled reports whether the build ended because it was cancelled
// rather than because a step failed.
//
// Verified against the live v1.1 API: when CircleCI auto-cancels a running
// build (a newer pipeline started on the same branch, or a redundant
// workflow was detected) it records the build with status and outcome
// "failed" and the top-level canceled flag false -- indistinguishable from
// a real failure at that level. The steps tell the two apart: an
// auto-cancelled build has every action "success" up to the point of
// cancellation and only "canceled" actions from there on, while a real
// failure has an action with status "failed". A build cancelled before any
// step ran carries status/outcome "canceled" instead.
//
// The verdict is conservative: a build with a failed step that was cancelled
// afterwards, a timed-out step or an infrastructure failure is not an
// auto-cancel.
func (b *Build) AutoCancelled() bool {
	if b.Status == "canceled" || b.Outcome == "canceled" || b.Canceled {
		return true
	}
	sawCanceled := false
	for _, step := range b.Steps {
		for _, a := range step.Actions {
			switch {
			case a.Status == "canceled" || a.Canceled:
				sawCanceled = true
			case sawCanceled:
				// Something still ran (and did not get cancelled) after the
				// cancel point: not the trailing-cancel shape.
				return false
			case a.Status != "success":
				return false
			}
		}
	}
	return sawCanceled
}

// APIError is a non-2xx answer from the CircleCI API.
type APIError struct {
	StatusCode int
	// Message is the "message" field of the error body, when present.
	Message string
	// Authenticated is true when the request carried a token. Without one,
	// a 401/403/404 on a build usually means a private project, which the
	// error message points out.
	Authenticated bool
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.StatusCode)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if !e.Authenticated {
		switch e.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			msg += "; no CircleCI token configured (private project?)"
		}
	}
	return msg
}

// Client talks to the CircleCI v1.1 API. The zero value is not usable; use
// NewClient, or set BaseURL and HTTPClient explicitly (tests do).
type Client struct {
	HTTPClient *http.Client
	BaseURL    string
	// Token is sent as the Circle-Token header on every request when set.
	// Public projects can be read without one; private projects and the
	// retry endpoint require it.
	Token string
}

// NewClient returns a client for circleci.com whose token, if any, comes
// from LoadToken.
func NewClient() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		BaseURL:    DefaultBaseURL,
		Token:      LoadToken(),
	}
}

// HasToken reports whether requests will be authenticated.
func (c *Client) HasToken() bool {
	return c != nil && c.Token != ""
}

// LoadToken returns the CircleCI API token from the CIRCLECI_CLI_TOKEN
// environment variable or, failing that, from the token line of the
// CircleCI CLI's own config file (~/.circleci/cli.yml). It returns "" when
// neither is set, in which case only public projects can be inspected and
// no build can be retried.
func LoadToken() string {
	if tok := strings.TrimSpace(os.Getenv(tokenEnv)); tok != "" {
		return tok
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return tokenFromCLIConfig(filepath.Join(home, ".circleci", "cli.yml"))
}

// tokenFromCLIConfig reads the "token:" entry of a CircleCI CLI config file.
// The file is flat YAML; a line scan avoids pulling in a YAML dependency
// for one key.
func tokenFromCLIConfig(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "token:") {
			continue
		}
		return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "token:")), `"'`)
	}
	return ""
}

// Build fetches the v1.1 build behind ref.
func (c *Client) Build(ctx context.Context, ref BuildRef) (*Build, error) {
	return c.do(ctx, http.MethodGet, ref.path())
}

// Retry asks CircleCI to run the single build again on the same commit and
// returns the new build. The endpoint requires a token.
//
// A build retried this way runs on its own: the jobs that the workflow had
// left blocked or not run because of the cancel stay where they are. Prefer
// RerunWorkflowFromFailed whenever the build carries a workflow id.
func (c *Client) Retry(ctx context.Context, ref BuildRef) (*Build, error) {
	body, err := c.request(ctx, http.MethodPost, ref.path()+"/retry", nil)
	if err != nil {
		return nil, err
	}
	return decodeBuild(body)
}

// RerunWorkflowFromFailed asks CircleCI to run the workflow again from its
// failed jobs and returns the id of the new workflow run. Unlike Retry it
// also releases the jobs that depend on the failed one, so a repository
// whose branch protection requires those downstream contexts gets them.
// The endpoint requires a token.
func (c *Client) RerunWorkflowFromFailed(ctx context.Context, workflowID string) (string, error) {
	payload, err := json.Marshal(map[string]bool{"from_failed": true})
	if err != nil {
		return "", err
	}
	path := "/api/v2/workflow/" + url.PathEscape(workflowID) + "/rerun"
	body, err := c.request(ctx, http.MethodPost, path, payload)
	if err != nil {
		return "", err
	}
	var out struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(escapeControlChars(body), &out); err != nil {
		return "", fmt.Errorf("decoding rerun response: %w", err)
	}
	return out.WorkflowID, nil
}

func (c *Client) do(ctx context.Context, method, path string) (*Build, error) {
	body, err := c.request(ctx, method, path, nil)
	if err != nil {
		return nil, err
	}
	return decodeBuild(body)
}

// request performs one API call and returns the raw response body. A
// non-nil payload is sent as a JSON request body.
func (c *Client) request(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Circle-Token", c.Token)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{StatusCode: resp.StatusCode, Message: errorMessage(body), Authenticated: c.Token != ""}
	}
	return body, nil
}

// errorMessage extracts the "message" field CircleCI puts in error bodies.
func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(escapeControlChars(body), &e); err != nil {
		return strings.TrimSpace(string(body))
	}
	return e.Message
}

// decodeBuild parses a v1.1 build body leniently. The API emits raw control
// characters (terminal escapes, tabs) inside string values such as step
// commands, which encoding/json rejects as invalid JSON; they are rewritten
// into \u00XX escapes first. Only status fields are read, so the string
// contents themselves do not matter.
func decodeBuild(body []byte) (*Build, error) {
	var b Build
	if err := json.Unmarshal(escapeControlChars(body), &b); err != nil {
		return nil, fmt.Errorf("decoding build: %w", err)
	}
	return &b, nil
}

// escapeControlChars rewrites raw control characters inside JSON string
// literals into \u00XX escapes and leaves everything else (structure,
// whitespace between tokens, already-escaped sequences) untouched.
func escapeControlChars(in []byte) []byte {
	if !bytes.ContainsFunc(in, func(r rune) bool { return r < 0x20 }) {
		return in
	}
	out := make([]byte, 0, len(in)+64)
	inString, escaped := false, false
	for _, c := range in {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			case c < 0x20:
				out = append(out, fmt.Sprintf(`\u%04x`, c)...)
				continue
			}
		} else if c == '"' {
			inString = true
		}
		out = append(out, c)
	}
	return out
}
