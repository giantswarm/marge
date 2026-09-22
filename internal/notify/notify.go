// Package notify posts one sweep summary per team to the team's channel,
// through klaus-gateway's team-notice endpoint (POST /notices). The gateway
// owns the Slack app and the workspace credential; the sweep holds none.
//
// The caller authenticates as itself: the projected ServiceAccount token the
// pod already carries, for the audience the gateway reviews. The file is read
// per call because the kubelet rotates it.
//
// The summary is the run history of the scheduled sweep: no report repository
// and no database, so a channel that fills with runs that changed nothing
// stops being read. Silence is therefore part of the contract, and
// Client.Post refuses an empty message rather than posting a heartbeat.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// URLEnv carries the gateway's in-cluster base URL, TokenFileEnv the path of
// the projected ServiceAccount token.
const (
	URLEnv       = "MARGE_NOTICES_URL"
	TokenFileEnv = "MARGE_NOTICES_TOKEN_FILE"
)

// postTimeout bounds one post. A gateway that does not answer must not hold a
// sweep that has already done its work.
const postTimeout = 30 * time.Second

// TextMax is the gateway's own bound on a notice: one Slack section block,
// whose limit is 3000 characters. A longer summary is trimmed to whole lines
// rather than refused.
const TextMax = 3000

// noticeMargin keeps room for the line that says what was dropped.
const noticeMargin = 80

// channelID is the shape the gateway accepts. It rewrites and locates
// messages by ID, so it refuses a channel name.
var channelID = regexp.MustCompile(`^[CDG][A-Z0-9]{5,}$`)

// Client posts notices.
type Client struct {
	// BaseURL is the gateway as reached from the pod, without a trailing
	// path.
	BaseURL string
	// TokenFile holds the projected ServiceAccount token, read per post.
	TokenFile string
	HTTP      *http.Client
}

// LoadClient returns the client the environment configures, or nil when it
// names no gateway, which is not an error: a sweep run by hand posts nothing.
// The caller reports the summary on its own output either way.
func LoadClient() *Client {
	base := strings.TrimSpace(os.Getenv(URLEnv))
	if base == "" {
		return nil
	}
	return &Client{
		BaseURL:   strings.TrimRight(base, "/"),
		TokenFile: strings.TrimSpace(os.Getenv(TokenFileEnv)),
	}
}

// notice is the body of POST /notices. The gateway refuses unknown fields.
type notice struct {
	Team    string `json:"team"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// Post sends text to channel as team's notice. An empty channel or an empty
// text is an error: both mean the caller decided to post and then had nothing
// to say.
func (c *Client) Post(ctx context.Context, team, channel, text string) error {
	if !channelID.MatchString(strings.TrimSpace(channel)) {
		return fmt.Errorf("slack channel %q is not a channel ID: the team's policy must name one (C…)", channel)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("refusing to post an empty summary")
	}

	token, err := c.token()
	if err != nil {
		return err
	}
	body, err := json.Marshal(notice{Team: team, Channel: channel, Text: Fit(text)})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/notices", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("posting to Slack channel %s: %w", channel, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("posting to Slack channel %s: the gateway answered %s: %s", channel, resp.Status, answer(resp.Body))
	}
	return nil
}

// token reads the projected token, which the kubelet rotates, so it is read
// per post and never held.
func (c *Client) token() (string, error) {
	if c.TokenFile == "" {
		return "", fmt.Errorf("no ServiceAccount token file: set %s to the projected token the gateway reviews", TokenFileEnv)
	}
	raw, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("reading the ServiceAccount token %s: %w", c.TokenFile, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the ServiceAccount token %s is empty", c.TokenFile)
	}
	return token, nil
}

// Fit trims a summary to the gateway's bound on whole lines, and says how
// many lines it dropped. A summary that silently loses its last section
// reads as a sweep that did less than it did. It is exported so a caller
// that renders a summary can hold its own lines to what a notice carries.
func Fit(text string) string {
	if len(text) <= TextMax {
		return text
	}
	lines := strings.Split(text, "\n")
	kept, size := 0, 0
	for _, line := range lines {
		if size+len(line)+1 > TextMax-noticeMargin {
			break
		}
		size += len(line) + 1
		kept++
	}
	dropped := fmt.Sprintf("\n… %d more lines did not fit; the run's log has all of them.", len(lines)-kept)
	return strings.Join(lines[:kept], "\n") + dropped
}

// answer is the gateway's error text, which it writes as plain text.
func answer(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 1<<12))
	if err != nil {
		return "no reason given"
	}
	if reason := strings.TrimSpace(string(raw)); reason != "" {
		return reason
	}
	return "no reason given"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}
