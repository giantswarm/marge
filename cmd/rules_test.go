package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/remedy"
	"github.com/giantswarm/marge/internal/rules"
)

// A Node or Vite job colours its output and the Actions runner stamps every
// line, so a recorded excerpt carries escapes YAML forbids as content. The
// draft must still read back, or the first thing a person does with it is
// repair the fixture.
func TestWriteDraftReadsBack(t *testing.T) {
	dir := t.TempDir()
	rulesFlags.path = dir
	t.Cleanup(func() { rulesFlags.path = "rules" })

	group := &SweepUnhandled{
		Signature: "d6c0555bf440",
		Checks:    []string{"build", "ci/circleci: node-build"},
		Count:     1,
		PRs:       []string{"giantswarm/backstage#2250"},
		Excerpt: "2026-09-15T10:11:02.6059740Z \x1b[90m│\x1b[39m " +
			"\x1b[38;2;241;97;97mTypeError: Cannot read properties of undefined\x1b[39m\r\n" +
			"2026-09-15T10:11:02.6060980Z   at resolveTypescriptProject\n",
	}

	files := buildDraft("node-build-d6c055", group)
	require.NoError(t, writeDraft("node-build-d6c055", files))
	require.Len(t, files, 3)

	scenarios, err := rules.LoadScenarios(filepath.Join(dir, rules.ScenarioDir))
	require.NoError(t, err, "the drafted scenarios do not parse")
	require.Len(t, scenarios, 2)

	var recorded string
	for _, s := range scenarios {
		if s.Expect.Rule == "" {
			continue
		}
		for _, excerpt := range s.Subject.Logs {
			recorded = excerpt
		}
	}
	require.Contains(t, recorded, "TypeError: Cannot read properties of undefined")
	require.NotContains(t, recorded, "\x1b")
	require.NotContains(t, recorded, "2026-09-15T10:11")
}

// draftServer answers the five calls that open a draft pull request and
// records what was sent, so a test asserts the branch, the tree and the
// draft flag without a real repository.
type draftServer struct {
	tree   map[string]any
	commit map[string]any
	ref    map[string]any
	pull   map[string]any
	// refStatus is what the branch creation answers; 422 stands for a
	// branch that already exists.
	refStatus int
	openPulls string
}

func newDraftClient(t *testing.T, rec *draftServer) *github.Client {
	t.Helper()
	if rec.refStatus == 0 {
		rec.refStatus = http.StatusCreated
	}
	if rec.openPulls == "" {
		rec.openPulls = "[]"
	}
	decode := func(r *http.Request) map[string]any {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		return body
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			_, _ = w.Write([]byte(`{"ref":"refs/heads/main","object":{"sha":"basesha"}}`))
		case strings.Contains(r.URL.Path, "/git/commits/basesha"):
			_, _ = w.Write([]byte(`{"sha":"basesha","tree":{"sha":"basetree"}}`))
		case strings.HasSuffix(r.URL.Path, "/git/trees"):
			rec.tree = decode(r)
			_, _ = w.Write([]byte(`{"sha":"newtree"}`))
		case strings.HasSuffix(r.URL.Path, "/git/commits"):
			rec.commit = decode(r)
			_, _ = w.Write([]byte(`{"sha":"newcommit"}`))
		case strings.HasSuffix(r.URL.Path, "/git/refs"):
			rec.ref = decode(r)
			if rec.refStatus != http.StatusCreated {
				http.Error(w, `{"message":"Reference already exists"}`, rec.refStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ref":"refs/heads/rule/x"}`))
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(rec.openPulls))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			rec.pull = decode(r)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":9,"html_url":"https://github.com/giantswarm/marge/pull/9"}`))
		default:
			http.Error(w, `{"message":"unexpected `+r.Method+" "+r.URL.Path+`"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	baseURL := server.URL + "/"
	client, err := github.NewClient(github.WithHTTPClient(server.Client()), github.WithURLs(&baseURL, &baseURL))
	require.NoError(t, err)
	return client
}

func draftGroup() *SweepUnhandled {
	return &SweepUnhandled{
		Signature: "d6c0555bf440",
		Checks:    []string{"build"},
		Count:     2,
		PRs:       []string{"giantswarm/backstage#2250", "giantswarm/marge#107"},
		Excerpt:   "TypeError: Cannot read properties of undefined\n",
	}
}

// marge has no git workspace and no push credential, so the draft reaches
// GitHub as a tree, a commit, a branch and a pull request made through the
// API.
func TestOpenDraftPR(t *testing.T) {
	rec := &draftServer{}
	client := newDraftClient(t, rec)
	files := buildDraft("node-build-d6c055", draftGroup())

	pull, err := openDraftPR(t.Context(), client, "giantswarm", "marge", "main",
		"node-build-d6c055", files, draftGroup())

	require.NoError(t, err)
	require.Equal(t, "https://github.com/giantswarm/marge/pull/9", pull.GetHTMLURL())

	require.Equal(t, "basetree", rec.tree["base_tree"])
	paths := make([]string, 0, 3)
	for _, entry := range rec.tree["tree"].([]any) {
		paths = append(paths, entry.(map[string]any)["path"].(string))
	}
	require.Equal(t, []string{
		"rules/node-build-d6c055.yaml",
		"rules/testdata/node-build-d6c055/matches.yaml",
		"rules/testdata/node-build-d6c055/refuses.yaml",
	}, paths, "the pull request writes the catalogue's own paths, whatever --path named")

	require.Equal(t, "refs/heads/rule/node-build-d6c055", rec.ref["ref"])
	require.Equal(t, "newcommit", rec.ref["sha"])
	require.Equal(t, "rule/node-build-d6c055", rec.pull["head"])
	require.Equal(t, "main", rec.pull["base"])
	require.Equal(t, true, rec.pull["draft"], "a drafted rule is never a ready pull request")
	require.Contains(t, rec.pull["title"], "feat(rules): draft")
	require.Contains(t, rec.pull["body"], "d6c0555bf440")
}

// The skeleton does not validate, which is the point: a person names the
// action before the catalogue carries the rule.
func TestOpenDraftPRCarriesABlankAction(t *testing.T) {
	rec := &draftServer{}
	client := newDraftClient(t, rec)
	files := buildDraft("node-build-d6c055", draftGroup())

	_, err := openDraftPR(t.Context(), client, "giantswarm", "marge", "main",
		"node-build-d6c055", files, draftGroup())
	require.NoError(t, err)

	var rule string
	for _, entry := range rec.tree["tree"].([]any) {
		if entry.(map[string]any)["path"] == "rules/node-build-d6c055.yaml" {
			rule = entry.(map[string]any)["content"].(string)
		}
	}
	require.Contains(t, rule, `name: ""`)

	_, err = rules.Parse("node-build-d6c055.yaml", []byte(rule), remedy.Default())
	require.Error(t, err, "the drafted rule must not validate until a person names the action")
}

// A second draft of the same pattern reports the pull request already
// standing on the branch rather than opening another one.
func TestOpenDraftPRReportsAnExistingBranch(t *testing.T) {
	rec := &draftServer{
		refStatus: http.StatusUnprocessableEntity,
		openPulls: `[{"number":4,"html_url":"https://github.com/giantswarm/marge/pull/4"}]`,
	}
	client := newDraftClient(t, rec)
	files := buildDraft("node-build-d6c055", draftGroup())

	pull, err := openDraftPR(t.Context(), client, "giantswarm", "marge", "main",
		"node-build-d6c055", files, draftGroup())

	require.NoError(t, err)
	require.Equal(t, 4, pull.GetNumber())
	require.Nil(t, rec.pull, "no second pull request is opened")
}
