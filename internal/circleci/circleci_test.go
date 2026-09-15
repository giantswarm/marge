package circleci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are recorded v1.1 responses from giantswarm/mcp-capi (2026-09-
// 09/10), trimmed to the fields marge reads plus a raw ESC byte inside a
// step command so the lenient decoder is exercised on every load:
//
//	build-883-failed.json          a real failure: govulncheck step failed
//	build-1244-auto-cancelled.json auto-cancelled; Renovate pushed a newer head
//	build-1263-auto-cancelled.json auto-cancelled on the PR's current head
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

func TestParseBuildURL(t *testing.T) {
	tests := []struct {
		raw  string
		want BuildRef
		ok   bool
	}{
		{"https://circleci.com/gh/giantswarm/mcp-capi/1263", BuildRef{"github", "giantswarm", "mcp-capi", 1263}, true},
		{"https://circleci.com/gh/giantswarm/mcp-capi/1263?utm_campaign=vcs-integration-link", BuildRef{"github", "giantswarm", "mcp-capi", 1263}, true},
		{"https://circleci.com/bb/team/repo/7", BuildRef{"bitbucket", "team", "repo", 7}, true},
		{"https://app.circleci.com/pipelines/github/giantswarm/mcp-capi/512/workflows/ea42abad-ba5a-4169-854b-001d55b79c1a/jobs/1263", BuildRef{"github", "giantswarm", "mcp-capi", 1263}, true},
		{"https://circleci.com/gh/giantswarm/mcp-capi", BuildRef{}, false},
		{"https://circleci.com/gh/giantswarm/mcp-capi/abc", BuildRef{}, false},
		{"https://circleci.com/svn/giantswarm/mcp-capi/1", BuildRef{}, false},
		{"https://github.com/giantswarm/mcp-capi/actions/runs/1263", BuildRef{}, false},
		{"https://example.com/gh/giantswarm/mcp-capi/1263", BuildRef{}, false},
		{"", BuildRef{}, false},
		{"not a url", BuildRef{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := ParseBuildURL(tt.raw)
			if ok != tt.ok || got != tt.want {
				t.Errorf("ParseBuildURL(%q) = %+v, %v; want %+v, %v", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDecodeBuild_lenientOnControlChars(t *testing.T) {
	raw := loadFixture(t, "build-1263-auto-cancelled.json")

	// The fixture is deliberately not strict JSON.
	var strict Build
	if err := json.Unmarshal(raw, &strict); err == nil {
		t.Fatal("fixture should contain a raw control character that strict decoding rejects")
	}

	b, err := decodeBuild(raw)
	if err != nil {
		t.Fatalf("decodeBuild: %v", err)
	}
	if b.BuildNum != 1263 || b.VCSRevision != "7090e9230aeea78db24aa97c7903ef2d393f2c36" || b.Workflows.JobName != "go-build" {
		t.Errorf("decoded build = num %d rev %s job %s", b.BuildNum, b.VCSRevision, b.Workflows.JobName)
	}
	if len(b.Steps) != 21 {
		t.Errorf("steps = %d, want 21", len(b.Steps))
	}
}

func TestEscapeControlChars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean", `{"a":"b"}`, `{"a":"b"}`},
		{"escape in string", "{\"a\":\"x\x1b[1my\"}", `{"a":"x\u001b[1my"}`},
		{"tab and newline in string", "{\"a\":\"x\ty\nz\"}", `{"a":"x\u0009y\u000az"}`},
		{"whitespace between tokens kept", "{\n\t\"a\": 1\n}", "{\n\t\"a\": 1\n}"},
		{"escaped quote does not end string", "{\"a\":\"q\\\"\x01\"}", `{"a":"q\"\u0001"}`},
		{"escaped backslash then quote ends string", "{\"a\":\"\\\\\",\n\"b\":\"\x02\"}", "{\"a\":\"\\\\\",\n\"b\":\"\\u0002\"}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(escapeControlChars([]byte(tt.in))); got != tt.want {
				t.Errorf("escapeControlChars(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuild_AutoCancelled_fixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    bool
	}{
		{"build-883-failed.json", false},
		{"build-1244-auto-cancelled.json", true},
		{"build-1263-auto-cancelled.json", true},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			b, err := decodeBuild(loadFixture(t, tt.fixture))
			if err != nil {
				t.Fatalf("decodeBuild: %v", err)
			}
			// Every recorded build, cancelled or not, says "failed" at the
			// top level: the distinction has to come from the steps.
			if b.Status != "failed" || b.Outcome != "failed" || b.Canceled {
				t.Fatalf("fixture shape changed: status=%s outcome=%s canceled=%v", b.Status, b.Outcome, b.Canceled)
			}
			if got := b.AutoCancelled(); got != tt.want {
				t.Errorf("AutoCancelled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuild_AutoCancelled_shapes(t *testing.T) {
	step := func(statuses ...string) Step {
		s := Step{Name: "s"}
		for _, st := range statuses {
			s.Actions = append(s.Actions, Action{Status: st, Canceled: st == "canceled", Failed: st == "failed"})
		}
		return s
	}
	tests := []struct {
		name  string
		build Build
		want  bool
	}{
		{"cancelled before any step", Build{Status: "canceled", Outcome: "canceled", Canceled: true}, true},
		{"top-level canceled flag only", Build{Status: "failed", Canceled: true}, true},
		{"all success then canceled", Build{Status: "failed", Steps: []Step{step("success"), step("success"), step("canceled")}}, true},
		{"parallel containers cancelled", Build{Status: "failed", Steps: []Step{step("success", "success"), step("canceled", "canceled")}}, true},
		{"failed then canceled", Build{Status: "failed", Steps: []Step{step("success"), step("failed"), step("canceled")}}, false},
		{"canceled then something ran on", Build{Status: "failed", Steps: []Step{step("canceled"), step("success")}}, false},
		{"plain failure", Build{Status: "failed", Steps: []Step{step("success"), step("failed")}}, false},
		{"timed out", Build{Status: "failed", Steps: []Step{step("success"), step("timedout")}}, false},
		{"infrastructure fail", Build{Status: "failed", Steps: []Step{step("infrastructure_fail")}}, false},
		{"all green", Build{Status: "success", Steps: []Step{step("success"), step("success")}}, false},
		{"no steps at all", Build{Status: "failed"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.build.AutoCancelled(); got != tt.want {
				t.Errorf("AutoCancelled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeAPI serves the recorded fixtures on the v1.1 paths and records how
// each request authenticated.
type fakeAPI struct {
	t           *testing.T
	fixtures    map[string]string // path -> fixture file
	private     bool              // 404 unless a Circle-Token header is present
	retryCalls  []string
	rerunCalls  []string
	rerunBodies []string
	rerunTypes  []string
	tokens      []string
	rawQueries  []string
}

func (f *fakeAPI) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.tokens = append(f.tokens, r.Header.Get("Circle-Token"))
		f.rawQueries = append(f.rawQueries, r.URL.RawQuery)
		if _, _, ok := r.BasicAuth(); ok {
			f.t.Errorf("token must never travel as basic auth: %s %s", r.Method, r.URL.Path)
		}
		if f.private && r.Header.Get("Circle-Token") == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("{\n    \"message\": \"Build not found\"\n}"))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rerun") {
			body, _ := io.ReadAll(r.Body)
			f.rerunCalls = append(f.rerunCalls, r.URL.Path)
			f.rerunBodies = append(f.rerunBodies, string(body))
			f.rerunTypes = append(f.rerunTypes, r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"workflow_id":"9b9c4a0e-0000-4000-8000-000000000001"}`))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/retry") {
			f.retryCalls = append(f.retryCalls, r.URL.Path)
			_, _ = w.Write([]byte(`{"build_num": 1272, "status": "queued", "lifecycle": "queued", "vcs_revision": "7090e9230aeea78db24aa97c7903ef2d393f2c36", "retry_of": 1263}`))
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fixture, ok := f.fixtures[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Build not found"}`))
			return
		}
		_, _ = w.Write(loadFixture(f.t, fixture))
	})
	return httptest.NewServer(mux)
}

func TestClient_Build_publicWithoutToken(t *testing.T) {
	api := &fakeAPI{t: t, fixtures: map[string]string{"/api/v1.1/project/github/giantswarm/mcp-capi/1263": "build-1263-auto-cancelled.json"}}
	srv := api.server()
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	b, err := c.Build(context.Background(), BuildRef{VCS: "github", Owner: "giantswarm", Repo: "mcp-capi", Num: 1263})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.BuildNum != 1263 || !b.AutoCancelled() {
		t.Errorf("build = %+v, want auto-cancelled 1263", b)
	}
	if len(api.tokens) != 1 || api.tokens[0] != "" {
		t.Errorf("tokens sent = %q, want one empty header", api.tokens)
	}
}

func TestClient_Build_tokenTravelsAsHeaderOnly(t *testing.T) {
	api := &fakeAPI{t: t, private: true, fixtures: map[string]string{"/api/v1.1/project/github/giantswarm/private/883": "build-883-failed.json"}}
	srv := api.server()
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL, Token: "s3cret"}
	b, err := c.Build(context.Background(), BuildRef{VCS: "github", Owner: "giantswarm", Repo: "private", Num: 883})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.AutoCancelled() {
		t.Error("build 883 is a real failure")
	}
	if len(api.tokens) != 1 || api.tokens[0] != "s3cret" {
		t.Errorf("tokens sent = %q, want the token as Circle-Token header", api.tokens)
	}
	for _, q := range api.rawQueries {
		if strings.Contains(q, "s3cret") || strings.Contains(q, "circle-token") {
			t.Errorf("token leaked into the query string: %q", q)
		}
	}
}

func TestClient_Build_privateWithoutTokenSaysSo(t *testing.T) {
	api := &fakeAPI{t: t, private: true}
	srv := api.server()
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	_, err := c.Build(context.Background(), BuildRef{VCS: "github", Owner: "giantswarm", Repo: "private", Num: 1})
	if err == nil {
		t.Fatal("Build on a private project without a token should fail")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.StatusCode != http.StatusNotFound || apiErr.Message != "Build not found" || apiErr.Authenticated {
		t.Fatalf("err = %#v, want unauthenticated 404 APIError with the API message", err)
	}
	if !strings.Contains(err.Error(), "no CircleCI token") {
		t.Errorf("error %q should hint at the missing token", err)
	}

	// With a (wrong) token the hint would mislead: it must disappear.
	authed := &APIError{StatusCode: 401, Message: "Invalid token provided.", Authenticated: true}
	if strings.Contains(authed.Error(), "no CircleCI token") {
		t.Errorf("authenticated error %q must not claim the token is missing", authed)
	}
}

func TestClient_Retry(t *testing.T) {
	api := &fakeAPI{t: t, private: true}
	srv := api.server()
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL, Token: "s3cret"}
	nb, err := c.Retry(context.Background(), BuildRef{VCS: "github", Owner: "giantswarm", Repo: "mcp-capi", Num: 1263})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if nb.BuildNum != 1272 {
		t.Errorf("new build = %d, want 1272", nb.BuildNum)
	}
	want := []string{"/api/v1.1/project/github/giantswarm/mcp-capi/1263/retry"}
	if len(api.retryCalls) != 1 || api.retryCalls[0] != want[0] {
		t.Errorf("retry calls = %q, want %q", api.retryCalls, want)
	}
}

func TestClient_RerunWorkflowFromFailed(t *testing.T) {
	api := &fakeAPI{t: t, private: true}
	srv := api.server()
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL, Token: "s3cret"}
	if err := c.RerunWorkflowFromFailed(context.Background(), "ea42abad-ba5a-4169-854b-001d55b79c1a"); err != nil {
		t.Fatalf("RerunWorkflowFromFailed: %v", err)
	}
	want := "/api/v2/workflow/ea42abad-ba5a-4169-854b-001d55b79c1a/rerun"
	if len(api.rerunCalls) != 1 || api.rerunCalls[0] != want {
		t.Fatalf("rerun calls = %q, want %q", api.rerunCalls, want)
	}
	if api.rerunBodies[0] != `{"from_failed":true}` {
		t.Errorf("rerun body = %q, want from_failed so the blocked downstream jobs also run", api.rerunBodies[0])
	}
	if api.rerunTypes[0] != "application/json" {
		t.Errorf("rerun Content-Type = %q", api.rerunTypes[0])
	}
	if len(api.tokens) != 1 || api.tokens[0] != "s3cret" {
		t.Errorf("tokens sent = %q, want the token as Circle-Token header", api.tokens)
	}
	for _, q := range api.rawQueries {
		if strings.Contains(q, "s3cret") {
			t.Errorf("token leaked into the query string: %q", q)
		}
	}
}

func TestClient_RerunWorkflowFromFailed_needsToken(t *testing.T) {
	api := &fakeAPI{t: t, private: true}
	srv := api.server()
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	if err := c.RerunWorkflowFromFailed(context.Background(), "ea42abad-ba5a-4169-854b-001d55b79c1a"); err == nil {
		t.Fatal("rerun without a token should fail")
	}
	if len(api.rerunCalls) != 0 {
		t.Errorf("rerun reached the endpoint %q without a token", api.rerunCalls)
	}
}

func TestClient_BuildCarriesWorkflowID(t *testing.T) {
	api := &fakeAPI{t: t, fixtures: map[string]string{"/api/v1.1/project/github/giantswarm/mcp-capi/1263": "build-1263-auto-cancelled.json"}}
	srv := api.server()
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	b, err := c.Build(context.Background(), BuildRef{VCS: "github", Owner: "giantswarm", Repo: "mcp-capi", Num: 1263})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.Workflows.WorkflowID != "ea42abad-ba5a-4169-854b-001d55b79c1a" {
		t.Errorf("workflow id = %q", b.Workflows.WorkflowID)
	}
	if b.Workflows.WorkflowName != "build" {
		t.Errorf("workflow name = %q, want build", b.Workflows.WorkflowName)
	}
}

func TestLoadToken_sources(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cli.yml")
	if err := os.WriteFile(cfg, []byte("host: https://circleci.com\nendpoint: graphql-unstable\ntoken: \"from-file\"\nrest_endpoint: api/v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tokenFromCLIConfig(cfg); got != "from-file" {
		t.Errorf("tokenFromCLIConfig = %q, want from-file", got)
	}
	if got := tokenFromCLIConfig(filepath.Join(dir, "missing.yml")); got != "" {
		t.Errorf("tokenFromCLIConfig(missing) = %q, want empty", got)
	}

	t.Setenv(tokenEnv, " from-env ")
	if got := LoadToken(); got != "from-env" {
		t.Errorf("LoadToken with env = %q, want from-env (trimmed)", got)
	}
}

func TestClient_HasToken_nilSafe(t *testing.T) {
	var c *Client
	if c.HasToken() {
		t.Error("nil client must report no token")
	}
	if (&Client{}).HasToken() {
		t.Error("empty client must report no token")
	}
	if !(&Client{Token: "x"}).HasToken() {
		t.Error("client with token must report it")
	}
}
