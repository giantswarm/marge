package github

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewCallerClient_neverFallsBackToTheMachineIdentity is the guard the
// served mode rests on: a call that carries no bearer is refused with the
// sign-in error even when the environment holds a token and a complete App
// credential, either of which NewClient would happily authenticate as.
func TestNewCallerClient_neverFallsBackToTheMachineIdentity(t *testing.T) {
	_, keyPEM := testKey(t)
	keyFile := filepath.Join(t.TempDir(), "private-key.pem")
	require.NoError(t, os.WriteFile(keyFile, []byte(keyPEM), 0o600))

	clearAppEnv(t)
	t.Setenv("GITHUB_TOKEN", "a-token-of-the-process")
	t.Setenv(appIDEnv, "1234")
	t.Setenv(appInstallationIDEnv, "5678")
	t.Setenv(appPrivateKeyFileEnv, keyFile)

	client, err := NewClient(t.Context())
	require.NoError(t, err, "the App credential must be usable, or this test proves nothing")
	require.NotNil(t, client)

	client, err = NewCallerClient(t.Context())
	require.ErrorIs(t, err, ErrNoCallerToken)
	require.Nil(t, client)
	require.Contains(t, err.Error(), "Sign in")
}

// TestNewCallerClient_authenticatesAsTheCaller checks that the token the
// caller presented is the one GitHub sees.
func TestNewCallerClient_authenticatesAsTheCaller(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"login":"a-person"}`))
	}))
	t.Cleanup(server.Close)

	ctx := ContextWithCallerToken(t.Context(), "the-callers-grant")
	client, err := NewCallerClient(ctx)
	require.NoError(t, err)

	response, err := client.Client().Get(server.URL + "/user")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, "Bearer the-callers-grant", seen)
}

// TestContextWithBearer covers what arrives on the wire: muster sends the
// grant as a Bearer credential, a header in another scheme is not a token,
// and an absent one leaves the context as it was.
func TestContextWithBearer(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"no header", "", ""},
		{"bearer", "Bearer gho_the-grant", "gho_the-grant"},
		{"lowercase scheme", "bearer gho_the-grant", "gho_the-grant"},
		{"surrounding space", "  Bearer   gho_the-grant  ", "gho_the-grant"},
		{"basic is not a bearer", "Basic dXNlcjpwYXNz", ""},
		{"token scheme is not a bearer", "token gho_the-grant", ""},
		{"scheme without a credential", "Bearer", ""},
		{"empty credential", "Bearer   ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			require.Equal(t, tt.want, CallerToken(ContextWithBearer(t.Context(), request)))
		})
	}
}
