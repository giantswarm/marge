package process

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
	"github.com/giantswarm/marge/internal/remedy"
)

// rescueCandidate records a PR as a rescue candidate when every
// precondition the PR alone decides holds: a failure no rule recognised on
// a trusted bot PR, no security failure, rescues on for the repository, no
// rescue marker standing for the change, and every required check reported.
// A refusal is a note on the entry; a PR outside the rescue path gets none.
func (p *Processor) rescueCandidate(ctx context.Context, run *prRun) {
	if !p.Actions.Selected(ActionRescue) {
		return
	}
	entry := run.status.Entry(run.idx)
	if entry.Unhandled == nil || entry.State != pr.StatusFailed {
		return
	}
	if reason := p.rescueRefusal(ctx, run, entry.Policy); reason != "" {
		run.note("rescue refused: " + reason)
		return
	}
	urls := make(map[string]string, len(entry.Unhandled.Checks))
	for _, check := range entry.Unhandled.Checks {
		if url := run.checkURL(check); url != "" {
			urls[check] = url
		}
	}
	run.status.SetCandidate(run.idx, &pr.RescueCandidate{
		HeadSHA:   run.pull.GetHead().GetSHA(),
		Checks:    entry.Unhandled.Checks,
		CheckURLs: urls,
		Signature: entry.Unhandled.Signature,
		Excerpt:   entry.Unhandled.Excerpt,
		Files:     p.diffFiles(ctx, run),
	})
}

// rescueRefusal names the first precondition the PR fails, or "".
func (p *Processor) rescueRefusal(ctx context.Context, run *prRun, resolved *pr.Policy) string {
	switch {
	case resolved == nil || !resolved.Rescue.Enabled:
		return "rescues are off for this repository by policy"
	case run.kind == "":
		return "the author is not a trusted bot"
	case classifySecurityFailure(run.failing, p.securityPatterns()) != "":
		return "a security check failed"
	case len(run.required.Missing) > 0 || len(run.required.Pending) > 0 || run.checksPending:
		return "required checks have not all reported"
	}
	fresh := remedy.FreshRescue(run.rescueMarker(ctx, p), run.pull.GetHead().GetSHA(),
		func() pr.Fingerprint { return run.fingerprint(ctx, p) })
	if fresh != nil {
		return "a rescue already stands for this change: " + pr.FormatRescue(fresh, time.Now())
	}
	return ""
}

// HandoffOptions are the run-level facts of a handoff.
type HandoffOptions struct {
	Team        string
	TriggeredBy string
	// Toolchain is the language the run's image builds. Empty hands over
	// every language.
	Toolchain string
	Rescue    pr.RescuePolicy
	// Languages maps owner/repo to the language the team file declares.
	Languages map[string]string
	// Used counts the team's rescues of the last seven days.
	Used func(context.Context) (int, error)
}

// Handoff decides which candidates the run hands to the agent, and labels
// each one it hands over. It applies what the PR alone cannot decide: the
// team's weekly count, the PR's state just before the handoff, an open
// teammate PR on the same module and the run's toolchain. Candidates are
// taken in table order, so one repository's PRs go together.
//
// A weekly count that cannot be read hands nothing over.
func (p *Processor) Handoff(ctx context.Context, status *pr.PRStatus, opts HandoffOptions) *pr.Handoff {
	h := &pr.Handoff{
		Team:        opts.Team,
		TriggeredBy: opts.TriggeredBy,
		Toolchain:   opts.Toolchain,
		Budget: pr.HandoffBudget{
			Weekly:       opts.Rescue.Weekly,
			Timeout:      opts.Rescue.Timeout,
			PerRescueUSD: opts.Rescue.Budget.PerRescueUSD,
		},
	}
	candidates := status.CandidateIndices()
	if len(candidates) == 0 {
		return h
	}
	used, err := opts.Used(ctx)
	if err != nil {
		h.Error = "the team's weekly rescue count could not be read: " + err.Error()
		return h
	}
	h.Budget.Counted = true
	h.Budget.Used = used
	h.Budget.Remaining = max(0, opts.Rescue.Weekly-used)

	repos := make(map[string]*repoFacts)
	remaining := h.Budget.Remaining
	for _, idx := range candidates {
		entry := status.Entry(idx)
		key := entry.PR.Owner + "/" + entry.PR.Repo
		facts := repos[key]
		if facts == nil {
			facts = &repoFacts{owner: entry.PR.Owner, repo: entry.PR.Repo, teamLanguage: opts.Languages[key]}
			repos[key] = facts
		}
		if remaining == 0 {
			p.refuse(h, status, idx, fmt.Sprintf("the team's weekly count of %d is spent", opts.Rescue.Weekly))
			continue
		}
		pull, reason := p.stillRescuable(ctx, entry)
		if reason == "" {
			reason = p.toolchainRefusal(ctx, facts, opts.Toolchain)
		}
		if reason == "" {
			reason = p.teammateRefusal(ctx, facts, entry.Candidate)
		}
		if reason != "" {
			p.refuse(h, status, idx, reason)
			continue
		}
		remaining--
		p.handOver(ctx, h, status, idx, pull, facts)
	}
	return h
}

// repoFacts are what the handoff reads about a repository, once.
type repoFacts struct {
	owner, repo  string
	teamLanguage string

	languageRead bool
	language     string
	languageNote string

	teammatesRead bool
	teammates     []teammatePR
	teammatesErr  error
}

// teammatePR is an open PR of a person, with the files it changes.
type teammatePR struct {
	number int
	files  []string
}

// stillRescuable re-reads the PR right before the handoff. A PR that closed,
// moved its head or ran into a conflict since the sweep read it is not the
// change the sweep classified.
func (p *Processor) stillRescuable(ctx context.Context, entry pr.StatusEntry) (*github.PullRequest, string) {
	pull, _, err := p.Client.PullRequests.Get(ctx, entry.PR.Owner, entry.PR.Repo, entry.PR.Number)
	switch {
	case err != nil:
		return nil, ghErrorDetail("re-reading the PR failed", err)
	case pull.GetMerged() || pull.GetState() != "open":
		return nil, "the PR closed since the sweep read it"
	case !strings.EqualFold(pull.GetHead().GetSHA(), entry.Candidate.HeadSHA):
		return nil, "the head moved since the sweep read it; the next sweep classifies the new change"
	case pull.GetMergeableState() == mergeableDirty:
		return nil, "the PR has a merge conflict"
	}
	return pull, ""
}

// toolchainRefusal refuses a repository whose language the run's image
// does not build. An empty toolchain builds every language.
func (p *Processor) toolchainRefusal(ctx context.Context, facts *repoFacts, toolchain string) string {
	if toolchain == "" {
		return ""
	}
	language, _ := p.repoLanguage(ctx, facts)
	if strings.EqualFold(language, toolchain) {
		return ""
	}
	return fmt.Sprintf("the repository is %s and this run builds %s", languageName(language), toolchain)
}

// rootLanguages are the files at a repository root that say which
// toolchain builds it, in the order they are believed.
var rootLanguages = []struct{ file, language string }{
	{"go.mod", "go"},
	{"package.json", "node"},
	{"pyproject.toml", "python"},
	{"requirements.txt", "python"},
}

// repoLanguage returns the language of a repository: the team file's,
// unless the repository root names another toolchain, which then wins and
// the note says so. A wrong toolchain costs a rescue its budget installing
// one, so the root is believed over the file.
func (p *Processor) repoLanguage(ctx context.Context, facts *repoFacts) (string, string) {
	if facts.languageRead {
		return facts.language, facts.languageNote
	}
	facts.languageRead = true
	facts.language = facts.teamLanguage
	_, dir, resp, err := p.Client.Repositories.GetContents(ctx, facts.owner, facts.repo, "", nil)
	if err != nil {
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			facts.languageNote = ghErrorDetail("the repository root could not be read", err)
		}
		return facts.language, facts.languageNote
	}
	names := make(map[string]bool, len(dir))
	for _, item := range dir {
		names[item.GetName()] = true
	}
	for _, root := range rootLanguages {
		if !names[root.file] {
			continue
		}
		if root.language != facts.teamLanguage {
			facts.languageNote = fmt.Sprintf("the team file says %s, the repository root has %s", languageName(facts.teamLanguage), root.file)
			facts.language = root.language
		}
		break
	}
	return facts.language, facts.languageNote
}

func languageName(language string) string {
	if language == "" {
		return "no language"
	}
	return language
}

// teammateRefusal refuses a candidate while an open PR of a person changes
// a file under the directory of a manifest the candidate changes: two
// changes to one module at once leave one of them to be redone. A
// candidate whose diff could not be read collides with every teammate PR.
func (p *Processor) teammateRefusal(ctx context.Context, facts *repoFacts, candidate *pr.RescueCandidate) string {
	teammates, err := p.teammatePRs(ctx, facts)
	if err != nil {
		return ghErrorDetail("the open PRs could not be read", err)
	}
	modules := candidate.Modules()
	for _, teammate := range teammates {
		if candidate.Files == nil {
			return fmt.Sprintf("teammate PR #%d is open and the diff of this PR is unknown", teammate.number)
		}
		for _, file := range teammate.files {
			for _, module := range modules {
				if module == "." || strings.HasPrefix(file, module+"/") {
					return fmt.Sprintf("teammate PR #%d changes %s, in the module at %s", teammate.number, file, path.Clean(module))
				}
			}
		}
	}
	return ""
}

// teammatePRs lists the open PRs of the repository that no trusted bot
// authored, with their files, once per repository.
func (p *Processor) teammatePRs(ctx context.Context, facts *repoFacts) ([]teammatePR, error) {
	if facts.teammatesRead {
		return facts.teammates, facts.teammatesErr
	}
	facts.teammatesRead = true
	opts := &github.PullRequestListOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}}
	for {
		pulls, resp, err := p.Client.PullRequests.List(ctx, facts.owner, facts.repo, opts)
		if err != nil {
			facts.teammatesErr = err
			return nil, err
		}
		for _, pull := range pulls {
			if pr.KindOfPull(facts.owner, pull) != "" {
				continue
			}
			files, err := p.prFiles(ctx, facts.owner, facts.repo, pull.GetNumber())
			if err != nil {
				facts.teammatesErr = err
				return nil, err
			}
			facts.teammates = append(facts.teammates, teammatePR{number: pull.GetNumber(), files: files})
		}
		if resp.NextPage == 0 {
			return facts.teammates, nil
		}
		opts.Page = resp.NextPage
	}
}

func (p *Processor) prFiles(ctx context.Context, owner, repo string, number int) ([]string, error) {
	var files []string
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := p.Client.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, err
		}
		for _, f := range page {
			files = append(files, f.GetFilename())
		}
		if resp.NextPage == 0 {
			return files, nil
		}
		opts.Page = resp.NextPage
	}
}

// refuse records a candidate the run did not hand over.
func (p *Processor) refuse(h *pr.Handoff, status *pr.PRStatus, idx int, reason string) {
	entry := status.Entry(idx)
	h.Refused = append(h.Refused, pr.HandoffRefusal{PR: entry.PR, Reason: reason})
	status.Update(idx, entry.State, withNote(entry.Detail, "rescue refused: "+reason))
}

// handOver adds a candidate to the handoff and labels it rescued. The
// label is display only; the agent writes the marker that records the
// attempt.
func (p *Processor) handOver(ctx context.Context, h *pr.Handoff, status *pr.PRStatus, idx int, pull *github.PullRequest, facts *repoFacts) {
	entry := status.Entry(idx)
	candidate := entry.Candidate
	key := facts.owner + "/" + facts.repo
	if len(h.Repos) == 0 || h.Repos[len(h.Repos)-1].Repo != key {
		language, note := p.repoLanguage(ctx, facts)
		h.Repos = append(h.Repos, pr.HandoffRepo{Repo: key, Language: language, LanguageNote: note})
	}
	repo := &h.Repos[len(h.Repos)-1]
	repo.PRs = append(repo.PRs, pr.HandoffPR{
		PR:          entry.PR,
		Kind:        entry.Kind,
		UpdateType:  entry.UpdateType,
		HeadSHA:     candidate.HeadSHA,
		WriteTarget: pr.RescueWriteTarget(entry.Kind),
		Checks:      slices.Clone(candidate.Checks),
		CheckURLs:   maps.Clone(candidate.CheckURLs),
		Signature:   candidate.Signature,
		Excerpt:     candidate.Excerpt,
	})

	note := "handed over for rescue"
	if p.DryRun {
		note = "dry-run: would hand over for rescue"
	} else {
		run := &prRun{info: entry.PR, pull: pull, status: status, idx: idx}
		p.setLabel(ctx, run, pr.RescuedClass)
		if len(run.notes) > 0 {
			note = withNote(note, strings.Join(run.notes, "; "))
		}
	}
	status.Update(idx, entry.State, withNote(entry.Detail, note))
}
