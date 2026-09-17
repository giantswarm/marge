package github

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/go-github/v92/github"
)

// bearerPrefix is the scheme muster sends the person's grant under. RFC 6750
// makes it case-insensitive.
const bearerPrefix = "bearer "

// ErrNoCallerToken is returned when a request carried no bearer token. It is
// never an invitation to authenticate as something else: a served call
// belongs to the person who made it, and no request is answered with the
// App's identity or with a token the process happens to hold.
var ErrNoCallerToken = errors.New("not signed in: this server acts as the person calling it and the request carried no GitHub token. Sign in to the marge MCP server (core_auth_login in muster) and retry")

type callerKey struct{}

// ContextWithBearer carries the request's bearer token on ctx. It is the
// mcp-go HTTP context hook of the streamable transport, so every tool call
// of a session sees the token that arrived with it.
func ContextWithBearer(ctx context.Context, r *http.Request) context.Context {
	return ContextWithCallerToken(ctx, bearerToken(r.Header.Get("Authorization")))
}

// ContextWithCallerToken carries token on ctx. An empty token is left off,
// so a later read cannot tell it from no header at all.
func ContextWithCallerToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, callerKey{}, token)
}

// CallerToken returns the caller's token, or "" when the request carried
// none.
func CallerToken(ctx context.Context) string {
	token, _ := ctx.Value(callerKey{}).(string)
	return token
}

// NewCallerClient returns a GitHub client authenticated with the token the
// caller presented, so every read and write lands as that person. It reads
// no environment variable and no App credential: the process may hold both,
// and either would make a write the bot's rather than the caller's.
func NewCallerClient(ctx context.Context) (*github.Client, error) {
	token := CallerToken(ctx)
	if token == "" {
		return nil, ErrNoCallerToken
	}
	return github.NewClient(github.WithAuthToken(token))
}

// bearerToken returns the credential of an Authorization header carrying the
// Bearer scheme, or "" for any other header.
func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(header[len(bearerPrefix):])
}
