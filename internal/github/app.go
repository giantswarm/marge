package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The environment variables that carry the App credential. The private key
// is read from a file when both are set: a projected Secret is a file, and a
// PEM in an environment variable shows up in every process listing.
const (
	appIDEnv             = "MARGE_GITHUB_APP_ID"
	appInstallationIDEnv = "MARGE_GITHUB_APP_INSTALLATION_ID"
	appPrivateKeyEnv     = "MARGE_GITHUB_APP_PRIVATE_KEY"
	appPrivateKeyFileEnv = "MARGE_GITHUB_APP_PRIVATE_KEY_FILE"
)

// appJWTLifetime is how long the App JWT that mints installation tokens is
// valid. GitHub refuses a JWT whose lifetime is longer than ten minutes.
const appJWTLifetime = 9 * time.Minute

// appJWTBackdate moves the JWT's "issued at" into the past, so a clock that
// runs a little ahead of GitHub's does not produce a token from the future.
const appJWTBackdate = 30 * time.Second

// tokenRenewal is how long before its expiry an installation token is
// replaced. GitHub gives one hour; a minute of margin covers a request that
// starts just before the expiry.
const tokenRenewal = time.Minute

// App is the sweep GitHub App's credential. It mints installation tokens on
// demand and holds no token beyond the life of the process.
type App struct {
	ID             int64
	InstallationID int64

	key *rsa.PrivateKey
}

// LoadApp returns the App credential the environment carries, or nil when it
// carries none, which is not an error: the token path serves a person
// running the CLI. A partial credential is an error, because it is a
// misconfigured unattended run and not a person's shell.
func LoadApp() (*App, error) {
	id := strings.TrimSpace(os.Getenv(appIDEnv))
	installation := strings.TrimSpace(os.Getenv(appInstallationIDEnv))
	keyPEM, err := privateKeyPEM()
	if err != nil {
		return nil, err
	}

	if id == "" && installation == "" && len(keyPEM) == 0 {
		return nil, nil
	}
	missing := missingAppSettings(id, installation, keyPEM)
	if len(missing) > 0 {
		return nil, fmt.Errorf("incomplete GitHub App credential: set %s", strings.Join(missing, ", "))
	}

	appID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s=%q is not a number", appIDEnv, id)
	}
	installationID, err := strconv.ParseInt(installation, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s=%q is not a number", appInstallationIDEnv, installation)
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, err
	}
	return &App{ID: appID, InstallationID: installationID, key: key}, nil
}

// missingAppSettings names the App settings that are not set.
func missingAppSettings(id, installation string, keyPEM []byte) []string {
	var missing []string
	if id == "" {
		missing = append(missing, appIDEnv)
	}
	if installation == "" {
		missing = append(missing, appInstallationIDEnv)
	}
	if len(keyPEM) == 0 {
		missing = append(missing, appPrivateKeyFileEnv+" or "+appPrivateKeyEnv)
	}
	return missing
}

// privateKeyPEM returns the private key from the file the environment names,
// or from the environment itself.
func privateKeyPEM() ([]byte, error) {
	if path := strings.TrimSpace(os.Getenv(appPrivateKeyFileEnv)); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", appPrivateKeyFileEnv, err)
		}
		return bytes.TrimSpace(data), nil
	}
	return []byte(strings.TrimSpace(os.Getenv(appPrivateKeyEnv))), nil
}

// parsePrivateKey reads the PEM GitHub hands out when a key is generated
// (PKCS#1), and the PKCS#8 form a conversion produces.
func parsePrivateKey(keyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("GitHub App private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing GitHub App private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("GitHub App private key is %T, and GitHub signs with RSA", parsed)
	}
	return key, nil
}

// JWT returns the App JWT that authenticates marge as the App itself, which
// is what the token mint and GET /app accept.
func (a *App) JWT(now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-appJWTBackdate).Unix(),
		"exp": now.Add(appJWTLifetime).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	}
	signing, err := signingInput(header, claims)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing the App JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func signingInput(header map[string]string, claims map[string]any) (string, error) {
	encoded := make([]string, 0, 2)
	for _, part := range []any{header, claims} {
		buf, err := json.Marshal(part)
		if err != nil {
			return "", fmt.Errorf("encoding the App JWT: %w", err)
		}
		encoded = append(encoded, base64.RawURLEncoding.EncodeToString(buf))
	}
	return encoded[0] + "." + encoded[1], nil
}

// installationToken is one minted token and the moment it stops working.
type installationToken struct {
	value   string
	expires time.Time
}

// installationTokenRequest is the body of the mint. An empty Repositories
// mints an installation-wide token, which only the calls that name no
// repository use.
type installationTokenRequest struct {
	Repositories []string `json:"repositories,omitempty"`
}

// MintToken returns an installation token for the named repositories. GitHub
// scopes the token to those repositories alone: every other repository of the
// installation answers 404 under it.
func (a *App) MintToken(ctx context.Context, httpClient *http.Client, baseURL string, repos []string) (string, time.Time, error) {
	jwt, err := a.JWT(time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	body, err := json.Marshal(installationTokenRequest{Repositories: repos})
	if err != nil {
		return "", time.Time{}, err
	}
	url := fmt.Sprintf("%sapp/installations/%d/access_tokens", baseURL, a.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("minting an installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("minting an installation token for %s: %s", scopeName(repos), resp.Status)
	}
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding the installation token: %w", err)
	}
	if minted.Token == "" {
		return "", time.Time{}, errors.New("GitHub returned an empty installation token")
	}
	return minted.Token, minted.ExpiresAt, nil
}

// scopeName names a token's scope for an error message.
func scopeName(repos []string) string {
	if len(repos) == 0 {
		return "the whole installation"
	}
	return strings.Join(repos, ", ")
}

// appTransport authenticates every request as the App. A request that names
// one repository carries a token scoped to that repository; the mint itself
// and GET /app carry the App JWT; everything else carries an
// installation-wide token, which is what a code search spans.
//
// Tokens live in this map and nowhere else. The process exits, the tokens go.
type appTransport struct {
	app     *App
	base    http.RoundTripper
	baseURL string
	// client mints tokens. It uses base directly, so a mint never recurses
	// through this transport.
	client *http.Client

	mu     sync.Mutex
	tokens map[string]installationToken
}

func (t *appTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if isAppRequest(req.URL.Path) {
		jwt, err := t.app.JWT(time.Now())
		if err != nil {
			return nil, err
		}
		return t.base.RoundTrip(authorized(req, "Bearer "+jwt))
	}
	owner, name := repositoryScope(req.URL.Path)
	token, err := t.token(req.Context(), owner, name)
	if err != nil {
		return nil, err
	}
	return t.base.RoundTrip(authorized(req, "Bearer "+token))
}

// token returns a live token for the scope, minting one when the scope has
// none or when the one it has is about to expire. An empty name is the
// installation-wide scope.
func (t *appTransport) token(ctx context.Context, owner, name string) (string, error) {
	key := owner + "/" + name
	var repos []string
	if name != "" {
		repos = []string{name}
	}

	t.mu.Lock()
	held, ok := t.tokens[key]
	t.mu.Unlock()
	if ok && time.Until(held.expires) > tokenRenewal {
		return held.value, nil
	}

	value, expires, err := t.app.MintToken(ctx, t.client, t.baseURL, repos)
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	t.tokens[key] = installationToken{value: value, expires: expires}
	t.mu.Unlock()
	return value, nil
}

// authorized copies the request and sets the Authorization header on the
// copy. A RoundTripper must not modify the request it is given.
func authorized(req *http.Request, value string) *http.Request {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", value)
	return clone
}

// isAppRequest reports whether the path is one the App JWT authenticates
// rather than an installation token: the App itself and its installations.
func isAppRequest(path string) bool {
	trimmed := strings.Trim(path, "/")
	return trimmed == "app" || strings.HasPrefix(trimmed, "app/")
}

// repositoryScope returns the owner and name of the single repository a path
// names. Both are empty for a path that names none: only
// /repos/{owner}/{name}/... names one, and a search spans the installation.
func repositoryScope(path string) (owner, name string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[1], parts[2]
}
