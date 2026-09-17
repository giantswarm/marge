// Package slack posts one sweep summary per team to the team's channel.
//
// The summary is the run history of the scheduled sweep: no report
// repository and no database, so a channel that fills with runs that changed
// nothing stops being read. Silence is therefore part of the contract, and
// Client.Post refuses an empty message rather than posting a heartbeat.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// TokenEnv carries the bot token of the sweep's own Slack app. The app holds
// chat:write and nothing else.
const TokenEnv = "MARGE_SLACK_TOKEN"

// defaultAPIURL is Slack's Web API root, with the trailing slash the method
// name is appended to.
const defaultAPIURL = "https://slack.com/api/"

// postTimeout bounds one post. A Slack that does not answer must not hold a
// sweep that has already done its work.
const postTimeout = 30 * time.Second

// Client posts messages with chat.postMessage.
type Client struct {
	Token string
	// APIURL overrides Slack's own root. Only the tests set it.
	APIURL string
	HTTP   *http.Client
}

// LoadClient returns the client the environment configures, or nil when it
// carries no token, which is not an error: a sweep run by hand posts
// nothing. The caller reports the summary on its own output either way.
func LoadClient() *Client {
	token := strings.TrimSpace(os.Getenv(TokenEnv))
	if token == "" {
		return nil
	}
	return &Client{Token: token}
}

// postRequest is the chat.postMessage body. The summary is plain text, so
// Slack's own link and code formatting applies and no block kit is needed.
type postRequest struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// postResponse is Slack's answer. Slack reports a failure with HTTP 200 and
// ok false, so the body is read on every status.
type postResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Post sends text to channel. An empty channel or an empty text is an error:
// both mean the caller decided to post and then had nothing to say.
func (c *Client) Post(ctx context.Context, channel, text string) error {
	if strings.TrimSpace(channel) == "" {
		return errors.New("no Slack channel: the team's policy names none")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("refusing to post an empty summary")
	}

	body, err := json.Marshal(postRequest{Channel: channel, Text: text})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL()+"chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("posting to Slack channel %s: %w", channel, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var answer postResponse
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return fmt.Errorf("decoding the Slack answer for channel %s: %w", channel, err)
	}
	if !answer.OK {
		return fmt.Errorf("posting to Slack channel %s: %s", channel, answer.Error)
	}
	return nil
}

func (c *Client) apiURL() string {
	if c.APIURL == "" {
		return defaultAPIURL
	}
	return c.APIURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}
