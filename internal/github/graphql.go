package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-github/v92/github"
)

// GraphQLURL returns the GraphQL endpoint of the API a client talks to.
// go-github speaks REST only, so the endpoint is derived from the REST root:
// github.com serves it at /graphql, and GitHub Enterprise Server next to the
// /api/v3 root it was given.
func GraphQLURL(client *github.Client) (string, error) {
	base, err := url.Parse(client.BaseURL())
	if err != nil {
		return "", fmt.Errorf("reading the API root: %w", err)
	}
	if base.Host == "api.github.com" {
		base.Path = "/graphql"
		return base.String(), nil
	}
	base.Path = strings.TrimSuffix(strings.TrimSuffix(base.Path, "/"), "/v3") + "/graphql"
	return base.String(), nil
}

// graphQLError is one entry of a GraphQL response's errors array. A field
// the query could not resolve is reported here and left null in the data,
// so a single unreadable repository does not fail the whole request.
type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	// Path names the field the error belongs to, alias first.
	Path []any `json:"path"`
}

// alias returns the response alias the error belongs to, or "" for an error
// that names no field.
func (e graphQLError) alias() string {
	if len(e.Path) == 0 {
		return ""
	}
	name, _ := e.Path[0].(string)
	return name
}

// graphQL posts one query and decodes its data into out. The GraphQL errors
// are returned alongside, because a response can carry both: GitHub answers
// 200 with the fields it could resolve and an error per field it could not.
// A returned error means the request itself failed, and out is then untouched.
func graphQL(ctx context.Context, client *github.Client, query string, variables map[string]any, out any) ([]graphQLError, error) {
	endpoint, err := GraphQLURL(client)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("encoding the query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")

	// The client's own http.Client carries the credential: the caller's
	// token, or the App transport, which mints an installation-wide token
	// for a path that names no repository.
	response, err := client.Client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, fmt.Errorf("graphql: %s: %s", response.Status, strings.TrimSpace(string(message)))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding the answer: %w", err)
	}
	if len(envelope.Data) > 0 && out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return envelope.Errors, fmt.Errorf("decoding the answer: %w", err)
		}
	}
	return envelope.Errors, nil
}
