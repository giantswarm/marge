package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
)

// rescueRun is a failing Renovate PR no rule recognised, under a policy
// that allows rescues.
func rescueRun() *prRun {
	run := remedyRun(pr.StatusFailed)
	policy := pr.CompanyDefaults()
	policy.Rescue.Enabled = true
	run.status.SetPolicy(run.idx, policy)
	run.status.SetUnhandled(run.idx, &pr.Unhandled{Signature: "abc123abc123", Checks: []string{"go-build"}, Excerpt: "undefined: foo"})
	run.files = []string{"go.mod", "go.sum"}
	run.detailsURLs = map[string]string{"go-build": "https://github.com/giantswarm/marge/actions/runs/1/job/2"}
	return run
}

func rescueProcessor() *Processor {
	return &Processor{
		Login:   "marge",
		Actions: ActionSet{ActionClassify: true, ActionRemedy: true, ActionRescue: true, ActionMark: true},
	}
}

func TestRescueCandidate_recordsWhatTheHandoffNeeds(t *testing.T) {
	run := rescueRun()

	rescueProcessor().rescueCandidate(t.Context(), run)

	candidate := run.status.Entry(run.idx).Candidate
	require.NotNil(t, candidate)
	require.Equal(t, "headsha", candidate.HeadSHA)
	require.Equal(t, []string{"go-build"}, candidate.Checks)
	require.Equal(t, "https://github.com/giantswarm/marge/actions/runs/1/job/2", candidate.CheckURLs["go-build"])
	require.Equal(t, "abc123abc123", candidate.Signature)
	require.Equal(t, []string{"."}, candidate.Modules())
	require.Empty(t, run.notes)
}

// Every precondition the PR alone decides refuses with its reason, and a PR
// outside the rescue path is left without a note.
func TestRescueCandidate_refusals(t *testing.T) {
	tests := []struct {
		name     string
		modify   func(p *Processor, run *prRun)
		wantNote string
	}{
		{
			name:   "not selected",
			modify: func(p *Processor, _ *prRun) { p.Actions = nil },
		},
		{
			name:   "a rule recognised the failure",
			modify: func(_ *Processor, run *prRun) { run.status.SetUnhandled(run.idx, nil) },
		},
		{
			name:   "not a failure",
			modify: func(_ *Processor, run *prRun) { run.status.Update(run.idx, pr.StatusStale, "") },
		},
		{
			name: "rescues off by policy",
			modify: func(_ *Processor, run *prRun) {
				policy := pr.CompanyDefaults()
				run.status.SetPolicy(run.idx, policy)
			},
			wantNote: "rescue refused: rescues are off for this repository by policy",
		},
		{
			name:     "untrusted author",
			modify:   func(_ *Processor, run *prRun) { run.kind = "" },
			wantNote: "rescue refused: the author is not a trusted bot",
		},
		{
			name:     "a security check failed",
			modify:   func(_ *Processor, run *prRun) { run.failing = append(run.failing, "trivy-scan") },
			wantNote: "rescue refused: a security check failed",
		},
		{
			name:     "a required check is pending",
			modify:   func(_ *Processor, run *prRun) { run.required = remedy.Required{Pending: []string{"ci"}} },
			wantNote: "rescue refused: required checks have not all reported",
		},
		{
			name:     "a required check never reported",
			modify:   func(_ *Processor, run *prRun) { run.required = remedy.Required{Missing: []string{"ci"}} },
			wantNote: "rescue refused: required checks have not all reported",
		},
		{
			name: "a rescue already stands for the change",
			modify: func(_ *Processor, run *prRun) {
				run.commentMarkers = []*pr.RescueMarker{{Tool: "rescue-agent", Outcome: pr.RescueOutcomeFailed, HeadSHA: "headsha"}}
			},
			wantNote: "rescue refused: a rescue already stands for this change: rescue failed (rescue-agent)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, run := rescueProcessor(), rescueRun()
			tt.modify(p, run)

			p.rescueCandidate(t.Context(), run)

			require.Nil(t, run.status.Entry(run.idx).Candidate)
			if tt.wantNote == "" {
				require.Empty(t, run.notes)
				return
			}
			require.Equal(t, []string{tt.wantNote}, run.notes)
		})
	}
}

// A rescue marker for a change that moved on blocks nothing.
func TestRescueCandidate_aStaleMarkerDoesNotRefuse(t *testing.T) {
	run := rescueRun()
	run.commentMarkers = []*pr.RescueMarker{{Tool: "rescue-agent", Outcome: pr.RescueOutcomeFailed, HeadSHA: "oldhead"}}

	rescueProcessor().rescueCandidate(t.Context(), run)

	require.NotNil(t, run.status.Entry(run.idx).Candidate)
}

// handoffGitHub fakes what the handoff reads and writes on one repository.
type handoffGitHub struct {
	mu sync.Mutex

	pulls     map[int]*github.PullRequest
	root      []string
	open      []*github.PullRequest
	openFiles map[int][]string
	labelled  map[int][]string
	reads     int
}

func newHandoffGitHub() *handoffGitHub {
	return &handoffGitHub{
		pulls:     map[int]*github.PullRequest{},
		openFiles: map[int][]string{},
		labelled:  map[int][]string{},
	}
}

func (g *handoffGitHub) addPull(number int, head string) {
	g.pulls[number] = &github.PullRequest{
		Number:         new(number),
		State:          new("open"),
		MergeableState: new("clean"),
		User:           &github.User{Login: new("renovate[bot]")},
		Head:           &github.PullRequestBranch{SHA: new(head)},
	}
}

func (g *handoffGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/giantswarm/marge/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.reads++
		var number int
		_ = json.Unmarshal([]byte(r.PathValue("number")), &number)
		pull, ok := g.pulls[number]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, pull)
	})
	mux.HandleFunc("GET /repos/giantswarm/marge/contents/", func(w http.ResponseWriter, _ *http.Request) {
		var dir []*github.RepositoryContent
		for _, name := range g.root {
			dir = append(dir, &github.RepositoryContent{Name: new(name), Type: new("file")})
		}
		writeJSON(w, dir)
	})
	mux.HandleFunc("GET /repos/giantswarm/marge/pulls", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, g.open)
	})
	mux.HandleFunc("GET /repos/giantswarm/marge/pulls/{number}/files", func(w http.ResponseWriter, r *http.Request) {
		var number int
		_ = json.Unmarshal([]byte(r.PathValue("number")), &number)
		var files []*github.CommitFile
		for _, name := range g.openFiles[number] {
			files = append(files, &github.CommitFile{Filename: new(name)})
		}
		writeJSON(w, files)
	})
	mux.HandleFunc("POST /repos/giantswarm/marge/issues/{number}/labels", func(w http.ResponseWriter, r *http.Request) {
		var labels []string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&labels))
		var number int
		_ = json.Unmarshal([]byte(r.PathValue("number")), &number)
		g.mu.Lock()
		g.labelled[number] = append(g.labelled[number], labels...)
		g.mu.Unlock()
		writeJSON(w, []*github.Label{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// candidates adds one rescue candidate per number to a fresh status.
func candidates(numbers ...int) *pr.PRStatus {
	status := pr.NewPRStatus()
	for _, number := range numbers {
		info := pr.PRInfo{Owner: "giantswarm", Repo: "marge", Number: number, URL: fmt.Sprintf("https://github.com/giantswarm/marge/pull/%d", number)}
		idx := status.Add(info)
		status.Update(idx, pr.StatusFailed, "go-build failed")
		status.SetClassification(idx, pr.KindRenovate, pr.UpdateMinor)
		status.SetCandidate(idx, &pr.RescueCandidate{
			HeadSHA:   "head",
			Checks:    []string{"go-build"},
			CheckURLs: map[string]string{"go-build": "https://example.invalid/job"},
			Signature: "abc123abc123",
			Excerpt:   "undefined: foo",
			Files:     []string{"go.mod", "go.sum"},
		})
	}
	return status
}

func handoffOptions(used int, err error) HandoffOptions {
	return HandoffOptions{
		Team:        "bumblebee",
		TriggeredBy: "the schedule",
		Rescue:      pr.RescuePolicy{Enabled: true, Weekly: 5, Timeout: 20 * time.Minute, Budget: pr.Budget{PerRescueUSD: 3}},
		Languages:   map[string]string{"giantswarm/marge": "go"},
		Used:        func(context.Context) (int, error) { return used, err },
	}
}

func TestHandoff_handsOverAndLabels(t *testing.T) {
	gh := newHandoffGitHub()
	gh.addPull(1, "head")
	gh.root = []string{"go.mod", "main.go"}
	p := rescueProcessor()
	p.Client = newTestClient(t, gh.server(t))
	status := candidates(1)

	h := p.Handoff(t.Context(), status, handoffOptions(2, nil))

	require.Empty(t, h.Error)
	require.Equal(t, pr.HandoffBudget{Weekly: 5, Counted: true, Used: 2, Remaining: 3, Timeout: 20 * time.Minute, PerRescueUSD: 3}, h.Budget)
	require.Equal(t, "the schedule", h.TriggeredBy)
	require.Len(t, h.Repos, 1)
	require.Equal(t, "giantswarm/marge", h.Repos[0].Repo)
	require.Equal(t, "go", h.Repos[0].Language)
	require.Empty(t, h.Repos[0].LanguageNote)
	require.Len(t, h.Repos[0].PRs, 1)
	handed := h.Repos[0].PRs[0]
	require.Equal(t, pr.WritePRBranch, handed.WriteTarget)
	require.Equal(t, "head", handed.HeadSHA)
	require.Equal(t, []string{"go-build"}, handed.Checks)
	require.Empty(t, h.Refused)

	require.Equal(t, []string{pr.LabelPrefix + pr.RescuedClass}, gh.labelled[1])
	require.Contains(t, status.Entry(0).Detail, "handed over for rescue")
}

// The weekly count bounds the handoff across candidates: the first PRs in
// table order are handed over and the rest are refused with the count.
func TestHandoff_stopsAtTheWeeklyCount(t *testing.T) {
	gh := newHandoffGitHub()
	gh.addPull(1, "head")
	gh.addPull(2, "head")
	gh.root = []string{"go.mod"}
	p := rescueProcessor()
	p.Client = newTestClient(t, gh.server(t))

	h := p.Handoff(t.Context(), candidates(1, 2), handoffOptions(4, nil))

	require.Equal(t, 1, h.Count())
	require.Equal(t, 1, h.Repos[0].PRs[0].PR.Number)
	require.Len(t, h.Refused, 1)
	require.Equal(t, 2, h.Refused[0].PR.Number)
	require.Equal(t, "the team's weekly count of 5 is spent", h.Refused[0].Reason)
}

// A spent week reads no PR: the refusal needs nothing from GitHub.
func TestHandoff_aSpentWeekReadsNothing(t *testing.T) {
	gh := newHandoffGitHub()
	p := rescueProcessor()
	p.Client = newTestClient(t, gh.server(t))

	h := p.Handoff(t.Context(), candidates(1), handoffOptions(5, nil))

	require.Zero(t, h.Count())
	require.Len(t, h.Refused, 1)
	require.Zero(t, gh.reads)
	require.Empty(t, gh.labelled)
}

// An unknown count is not a count under the cap.
func TestHandoff_anUnreadableCountHandsNothingOver(t *testing.T) {
	p := rescueProcessor()

	h := p.Handoff(t.Context(), candidates(1), handoffOptions(0, errors.New("search refused")))

	require.Zero(t, h.Count())
	require.Equal(t, "the team's weekly rescue count could not be read: search refused", h.Error)
}

func TestHandoff_noCandidateReadsNoCount(t *testing.T) {
	p := rescueProcessor()
	opts := handoffOptions(0, nil)
	opts.Used = func(context.Context) (int, error) {
		t.Error("a run without a candidate must not search for markers")
		return 0, nil
	}

	h := p.Handoff(t.Context(), pr.NewPRStatus(), opts)

	require.Zero(t, h.Count())
	require.Empty(t, h.Error)
	require.False(t, h.Budget.Counted)
}

// The handoff re-reads each PR, and a PR that moved since the sweep read it
// is not the change the sweep classified.
func TestHandoff_refusesAPRThatChangedSinceTheSweep(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*github.PullRequest)
		want   string
	}{
		{"closed", func(p *github.PullRequest) { p.State = new("closed") }, "the PR closed since the sweep read it"},
		{"merged", func(p *github.PullRequest) { p.Merged = new(true) }, "the PR closed since the sweep read it"},
		{"head moved", func(p *github.PullRequest) { p.Head.SHA = new("newhead") }, "the head moved since the sweep read it; the next sweep classifies the new change"},
		{"conflict", func(p *github.PullRequest) { p.MergeableState = new("dirty") }, "the PR has a merge conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newHandoffGitHub()
			gh.addPull(1, "head")
			tt.modify(gh.pulls[1])
			p := rescueProcessor()
			p.Client = newTestClient(t, gh.server(t))
			status := candidates(1)

			h := p.Handoff(t.Context(), status, handoffOptions(0, nil))

			require.Zero(t, h.Count())
			require.Equal(t, []pr.HandoffRefusal{{PR: status.Entry(0).PR, Reason: tt.want}}, h.Refused)
			require.Contains(t, status.Entry(0).Detail, "rescue refused: "+tt.want)
			require.Empty(t, gh.labelled)
		})
	}
}

// An open PR of a person on the same module is a hard stop; one on another
// module is not.
func TestHandoff_teammatePRs(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"same module", []string{"cmd/root.go"}, "teammate PR #9 changes cmd/root.go, in the module at ."},
		{"no teammate PR", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newHandoffGitHub()
			gh.addPull(1, "head")
			gh.root = []string{"go.mod"}
			gh.open = []*github.PullRequest{{Number: new(1), User: &github.User{Login: new("renovate[bot]")}}}
			if tt.files != nil {
				gh.open = append(gh.open, &github.PullRequest{Number: new(9), User: &github.User{Login: new("a-person")}})
				gh.openFiles[9] = tt.files
			}
			p := rescueProcessor()
			p.Client = newTestClient(t, gh.server(t))

			h := p.Handoff(t.Context(), candidates(1), handoffOptions(0, nil))

			if tt.want == "" {
				require.Equal(t, 1, h.Count())
				return
			}
			require.Zero(t, h.Count())
			require.Equal(t, tt.want, h.Refused[0].Reason)
		})
	}
}

// A candidate in a nested module collides only with work under that module.
func TestHandoff_aNestedModuleCollidesOnlyWithItself(t *testing.T) {
	gh := newHandoffGitHub()
	gh.addPull(1, "head")
	gh.open = []*github.PullRequest{{Number: new(9), User: &github.User{Login: new("a-person")}}}
	gh.openFiles[9] = []string{"tools/gen/main.go"}
	p := rescueProcessor()
	p.Client = newTestClient(t, gh.server(t))
	status := candidates(1)
	entry := status.Entry(0)
	entry.Candidate.Files = []string{"api/go.mod", "api/go.sum"}
	status.SetCandidate(0, entry.Candidate)

	h := p.Handoff(t.Context(), status, handoffOptions(0, nil))

	require.Equal(t, 1, h.Count())
}

// The repository root is believed over the team file, and the run's
// toolchain refuses a repository it does not build.
func TestHandoff_language(t *testing.T) {
	tests := []struct {
		name      string
		team      string
		root      []string
		toolchain string
		wantLang  string
		wantNote  string
		refused   string
	}{
		{name: "the team file agrees", team: "go", root: []string{"go.mod"}, wantLang: "go"},
		{name: "a generic entry with a go.mod is go", team: "generic", root: []string{"go.mod"}, wantLang: "go", wantNote: "the team file says generic, the repository root has go.mod"},
		{name: "no manifest keeps the team file", team: "generic", root: []string{"Chart.yaml"}, wantLang: "generic"},
		{name: "the toolchain matches", team: "go", root: []string{"go.mod"}, toolchain: "go", wantLang: "go"},
		{name: "the toolchain does not build it", team: "generic", root: []string{"package.json"}, toolchain: "go", refused: "the repository is node and this run builds go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newHandoffGitHub()
			gh.addPull(1, "head")
			gh.root = tt.root
			p := rescueProcessor()
			p.Client = newTestClient(t, gh.server(t))
			opts := handoffOptions(0, nil)
			opts.Languages = map[string]string{"giantswarm/marge": tt.team}
			opts.Toolchain = tt.toolchain

			h := p.Handoff(t.Context(), candidates(1), opts)

			if tt.refused != "" {
				require.Zero(t, h.Count())
				require.Equal(t, tt.refused, h.Refused[0].Reason)
				return
			}
			require.Equal(t, 1, h.Count())
			require.Equal(t, tt.wantLang, h.Repos[0].Language)
			require.Equal(t, tt.wantNote, h.Repos[0].LanguageNote)
		})
	}
}

func TestHandoff_dryRunWritesNoLabel(t *testing.T) {
	gh := newHandoffGitHub()
	gh.addPull(1, "head")
	gh.root = []string{"go.mod"}
	p := rescueProcessor()
	p.DryRun = true
	p.Client = newTestClient(t, gh.server(t))
	status := candidates(1)

	h := p.Handoff(t.Context(), status, handoffOptions(0, nil))

	require.Equal(t, 1, h.Count())
	require.Empty(t, gh.labelled)
	require.Contains(t, status.Entry(0).Detail, "dry-run: would hand over for rescue")
}

func TestRescueWriteTarget(t *testing.T) {
	require.Equal(t, pr.WriteDefaultBranch, pr.RescueWriteTarget(pr.KindAlignFiles))
	for _, kind := range []pr.Kind{pr.KindRenovate, pr.KindDependabot, pr.KindHerald, pr.KindUpstreamSync} {
		require.Equal(t, pr.WritePRBranch, pr.RescueWriteTarget(kind), string(kind))
	}
}
