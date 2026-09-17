package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

// testKey returns an RSA key and its PKCS#1 PEM, the form GitHub hands out.
func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return key, string(pem.EncodeToMemory(block))
}

// clearAppEnv unsets every App variable, so a test starts from an
// environment that carries no credential whatever the shell holds.
func clearAppEnv(t *testing.T) {
	t.Helper()
	for _, env := range []string{appIDEnv, appInstallationIDEnv, appPrivateKeyEnv, appPrivateKeyFileEnv} {
		t.Setenv(env, "")
	}
}

// TestLoadApp_absentIsNotAnError holds the rule that keeps the token path
// working: a person running the CLI sets no App variable, and that is not a
// misconfiguration.
func TestLoadApp_absentIsNotAnError(t *testing.T) {
	clearAppEnv(t)

	app, err := LoadApp()
	require.NoError(t, err)
	require.Nil(t, app)
}

// TestLoadApp_partialCredentialIsAnError refuses an unattended run that
// would silently fall back to whatever token the pod happens to carry.
func TestLoadApp_partialCredentialIsAnError(t *testing.T) {
	clearAppEnv(t)
	t.Setenv(appIDEnv, "4950078")

	_, err := LoadApp()
	require.ErrorContains(t, err, "incomplete GitHub App credential")
	require.ErrorContains(t, err, appInstallationIDEnv)
	require.ErrorContains(t, err, appPrivateKeyFileEnv)
}

// TestLoadApp_readsTheKeyFromAFile is the path the chart takes: the private
// key is a projected Secret and never an environment variable.
func TestLoadApp_readsTheKeyFromAFile(t *testing.T) {
	_, keyPEM := testKey(t)
	path := filepath.Join(t.TempDir(), "private-key.pem")
	require.NoError(t, os.WriteFile(path, []byte(keyPEM), 0o600))

	clearAppEnv(t)
	t.Setenv(appIDEnv, "4950078")
	t.Setenv(appInstallationIDEnv, "161842404")
	t.Setenv(appPrivateKeyFileEnv, path)

	app, err := LoadApp()
	require.NoError(t, err)
	require.NotNil(t, app)
	require.Equal(t, int64(4950078), app.ID)
	require.Equal(t, int64(161842404), app.InstallationID)
}

// TestLoadApp_rejectsAKeyThatIsNotPEM names the problem instead of failing
// later on the first mint.
func TestLoadApp_rejectsAKeyThatIsNotPEM(t *testing.T) {
	clearAppEnv(t)
	t.Setenv(appIDEnv, "1")
	t.Setenv(appInstallationIDEnv, "2")
	t.Setenv(appPrivateKeyEnv, "not a key")

	_, err := LoadApp()
	require.ErrorContains(t, err, "not PEM")
}

// TestAppJWT_verifies against the App's own public key, with the claims
// GitHub requires: the App ID as issuer and a lifetime under ten minutes.
func TestAppJWT_verifies(t *testing.T) {
	key, _ := testKey(t)
	app := &App{ID: 4950078, key: key}
	now := time.Now()

	token, err := app.JWT(now)
	require.NoError(t, err)

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature))

	claims := decodeSegment(t, parts[1])
	require.Equal(t, "4950078", claims["iss"])
	issued := int64(claims["iat"].(float64))
	expires := int64(claims["exp"].(float64))
	require.Less(t, issued, now.Unix()+1)
	require.LessOrEqual(t, expires-issued, int64(600))
}

func decodeSegment(t *testing.T, segment string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// appServer serves the mint and one repository call, and records what it was
// asked for, so the transport is tested against real protocol traffic.
type appServer struct {
	*httptest.Server
	mints        atomic.Int32
	mintedScopes []string
	seenTokens   []string
	expiry       time.Time
	// permissions is what the mint reports for the token it hands out. The
	// default is the production installation's set, measured on 2026-09-17.
	permissions map[string]string
}

func newAppServer(t *testing.T) *appServer {
	t.Helper()
	server := &appServer{
		expiry: time.Now().Add(time.Hour),
		permissions: map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"metadata":      "read",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/161842404/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer eyJ"), "the mint must carry the App JWT")
		var body installationTokenRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		scope := strings.Join(body.Repositories, ",")
		server.mintedScopes = append(server.mintedScopes, scope)
		count := server.mints.Add(1)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       "ghs_" + scope + "_" + string(rune('0'+count)),
			"expires_at":  server.expiry.Format(time.RFC3339),
			"permissions": server.permissions,
		})
	})
	mux.HandleFunc("GET /repos/giantswarm/marge", func(w http.ResponseWriter, r *http.Request) {
		server.seenTokens = append(server.seenTokens, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "marge"})
	})
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer eyJ"), "GET /app must carry the App JWT")
		_ = json.NewEncoder(w).Encode(map[string]any{"slug": "giantswarm-marge"})
	})
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		server.seenTokens = append(server.seenTokens, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})

	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func appClient(t *testing.T, server *appServer) (*App, *github.Client) {
	t.Helper()
	key, _ := testKey(t)
	app := &App{ID: 4950078, InstallationID: 161842404, key: key}
	client, err := NewAppClient(app, server.URL+"/", server.Client())
	require.NoError(t, err)
	return app, client
}

// TestAppClient_mintsOneTokenPerRepository is the acceptance criterion: a
// repository call carries a token scoped to that repository alone, and a
// second call on the same repository reuses it instead of minting again.
func TestAppClient_mintsOneTokenPerRepository(t *testing.T) {
	server := newAppServer(t)
	_, client := appClient(t, server)

	for range 2 {
		_, _, err := client.Repositories.Get(t.Context(), "giantswarm", "marge")
		require.NoError(t, err)
	}

	require.Equal(t, int32(1), server.mints.Load(), "the second call reuses the token")
	require.Equal(t, []string{"marge"}, server.mintedScopes)
	require.Len(t, server.seenTokens, 2)
	require.Equal(t, "Bearer ghs_marge_1", server.seenTokens[0])
	require.Equal(t, server.seenTokens[0], server.seenTokens[1])
}

// TestAppClient_searchTokenIsNotRepositoryScoped holds the one call that
// cannot name a repository: a search spans the installation, so its token
// must not be scoped to one repository.
func TestAppClient_searchTokenIsNotRepositoryScoped(t *testing.T) {
	server := newAppServer(t)
	_, client := appClient(t, server)

	_, _, err := client.Search.Issues(t.Context(), "is:pr is:open", nil)
	require.NoError(t, err)

	require.Equal(t, []string{""}, server.mintedScopes)
}

// TestAppClient_expiredTokenIsReplaced holds that a run longer than a token
// mints a new one rather than sending a dead token.
func TestAppClient_expiredTokenIsReplaced(t *testing.T) {
	server := newAppServer(t)
	server.expiry = time.Now().Add(10 * time.Second)
	_, client := appClient(t, server)

	for range 2 {
		_, _, err := client.Repositories.Get(t.Context(), "giantswarm", "marge")
		require.NoError(t, err)
	}

	require.Equal(t, int32(2), server.mints.Load())
}

// TestAuthenticatedLogin_underAppCredential returns the bot login. An
// installation token has no user behind it, so GET /user is never called.
func TestAuthenticatedLogin_underAppCredential(t *testing.T) {
	server := newAppServer(t)
	_, keyPEM := testKey(t)
	clearAppEnv(t)
	t.Setenv(appIDEnv, "4950078")
	t.Setenv(appInstallationIDEnv, "161842404")
	t.Setenv(appPrivateKeyEnv, keyPEM)

	_, client := appClient(t, server)

	login, err := AuthenticatedLogin(t.Context(), client)
	require.NoError(t, err)
	require.Equal(t, "giantswarm-marge[bot]", login)
}

// TestRepositoryScope reads the repository out of the paths marge calls, and
// nothing out of the paths that name none.
func TestRepositoryScope(t *testing.T) {
	for path, want := range map[string][2]string{
		"/repos/giantswarm/marge/pulls/1": {"giantswarm", "marge"},
		"/repos/giantswarm/marge":         {"giantswarm", "marge"},
		"/search/issues":                  {"", ""},
		"/user":                           {"", ""},
		"/repos":                          {"", ""},
	} {
		owner, name := repositoryScope(path)
		require.Equal(t, want[0], owner, path)
		require.Equal(t, want[1], name, path)
	}
}

// TestIsAppRequest separates the calls the App JWT authenticates from the
// calls an installation token does.
func TestIsAppRequest(t *testing.T) {
	require.True(t, isAppRequest("/app"))
	require.True(t, isAppRequest("/app/installations/1/access_tokens"))
	require.False(t, isAppRequest("/apps/giantswarm-marge"))
	require.False(t, isAppRequest("/repos/giantswarm/marge"))
}

// TestAppWriteAccess_readsTheMintedPermissions holds the answer marge's
// approval guard needs. GET /repos reports permissions.push: false under
// every installation token, so the permissions GitHub reports on the mint are
// the only description of what the token may do.
func TestAppWriteAccess_readsTheMintedPermissions(t *testing.T) {
	tests := []struct {
		name        string
		permissions map[string]string
		want        bool
	}{
		{"contents: write", map[string]string{"contents": "write", "pull_requests": "write"}, true},
		{"contents: read", map[string]string{"contents": "read", "pull_requests": "write"}, false},
		{"no contents at all", map[string]string{"pull_requests": "write"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newAppServer(t)
			server.permissions = tt.permissions
			_, client := appClient(t, server)

			check := AppWriteAccess(client)
			require.NotNil(t, check, "an App client must answer the App question")

			allowed, err := check(t.Context(), "giantswarm", "marge")
			require.NoError(t, err)
			require.Equal(t, tt.want, allowed)
		})
	}
}

// TestAppWriteAccess_reusesTheRepositoryToken keeps the check free: the sweep
// already holds a token for the repository it is about to approve.
func TestAppWriteAccess_reusesTheRepositoryToken(t *testing.T) {
	server := newAppServer(t)
	_, client := appClient(t, server)

	_, _, err := client.Repositories.Get(t.Context(), "giantswarm", "marge")
	require.NoError(t, err)

	allowed, err := AppWriteAccess(client)(t.Context(), "giantswarm", "marge")
	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, int32(1), server.mints.Load(), "the check must not mint a second token")
}

// TestAppWriteAccess_isNilForATokenClient leaves a person's run on the
// permissions.push path, which is the only answer a user token has.
func TestAppWriteAccess_isNilForATokenClient(t *testing.T) {
	client, err := github.NewClient(github.WithAuthToken("ghp_example"))
	require.NoError(t, err)
	require.Nil(t, AppWriteAccess(client))
}

// TestAppWriteAccess_mintFailureIsNotADenial keeps a broken credential apart
// from a proven refusal.
func TestAppWriteAccess_mintFailureIsNotADenial(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/161842404/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	key, _ := testKey(t)
	client, err := NewAppClient(&App{ID: 4950078, InstallationID: 161842404, key: key}, server.URL+"/", server.Client())
	require.NoError(t, err)

	_, err = AppWriteAccess(client)(t.Context(), "giantswarm", "marge")
	require.Error(t, err)
}
