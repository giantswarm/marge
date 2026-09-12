package github

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeGH puts a gh stand-in first on PATH that prints token, or fails when
// token is "".
func fakeGH(t *testing.T, token string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if token != "" {
		script = "#!/bin/sh\n[ \"$1 $2\" = \"auth token\" ] || exit 1\necho '" + token + "'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestLoadToken(t *testing.T) {
	tests := []struct {
		name        string
		githubToken string
		ghToken     string
		gh          string
		want        string
	}{
		{name: "GITHUB_TOKEN wins", githubToken: "ghp_a", ghToken: "ghp_b", gh: "gho_c", want: "ghp_a"},
		{name: "GH_TOKEN when GITHUB_TOKEN is unset", ghToken: "ghp_b", gh: "gho_c", want: "ghp_b"},
		{name: "whitespace-only env is unset", githubToken: "  ", ghToken: " \n", gh: "gho_c", want: "gho_c"},
		{name: "gh CLI login as last resort", gh: "gho_c\n", want: "gho_c"},
		{name: "nothing configured", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tt.githubToken)
			t.Setenv("GH_TOKEN", tt.ghToken)
			fakeGH(t, tt.gh)
			if got := LoadToken(context.Background()); got != tt.want {
				t.Errorf("LoadToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	fakeGH(t, "")
	if _, err := NewClient(context.Background()); err == nil {
		t.Fatal("NewClient() without any token: want error, got nil")
	}

	fakeGH(t, "gho_c")
	client, err := NewClient(context.Background())
	if err != nil {
		t.Fatalf("NewClient() with gh login: %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned nil client")
	}
}
