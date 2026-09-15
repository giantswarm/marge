package process

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
	"github.com/giantswarm/marge/internal/pr"
)

const (
	checkPollInterval = 15 * time.Second
	checkPollTimeout  = 5 * time.Minute

	mergeMaxRetries    = 3
	mergeRetryBaseWait = 10 * time.Second
)

var DefaultTrustedAuthors = map[string]bool{
	"renovate[bot]":   true,
	"dependabot[bot]": true,
}

type Processor struct {
	Client         *github.Client
	DryRun         bool
	MergeAutoMerge bool
	Login          string
	TrustedAuthors map[string]bool

	// SecurityCheckPatterns is the list of case-insensitive substrings used
	// to flag failing CI checks as security-related (e.g. govulncheck, Trivy,
	// CodeQL). A nil slice falls back to DefaultSecurityCheckPatterns; a
	// non-nil empty slice disables security classification entirely.
	SecurityCheckPatterns []string

	// MergeMaxRetries is the maximum number of merge attempts when the base
	// branch is modified between fetch and merge. Zero uses the default (3).
	MergeMaxRetries int
	// MergeRetryWait is the base wait duration between merge retries.
	// Zero uses the default (10s). Actual wait = base * attempt number.
	MergeRetryWait time.Duration

	// RefreshStale updates the branch of every stale PR from its base
	// (see classifyStale) so CI re-runs against current code. Without it a
	// stale PR is only reported as such. Ignored in dry-run mode.
	RefreshStale bool

	// CircleCI looks behind failing "ci/circleci: <job>" commit statuses to
	// tell an auto-cancelled build from a real failure (see
	// classifyCancelled). Nil disables the lookup and every CircleCI
	// failure is taken at face value.
	CircleCI *circleci.Client
	// RetryCancelled retries every auto-cancelled CircleCI build that ran on
	// a PR's current head so the same commit gets a real verdict. Without it
	// a cancelled PR is only reported as such. Ignored in dry-run mode.
	RetryCancelled bool

	staleCache
	accessCache
}

func NewProcessor(client *github.Client, dryRun bool, mergeAutoMerge bool, login string, trustedAuthors map[string]bool) *Processor {
	src := trustedAuthors
	if src == nil {
		src = DefaultTrustedAuthors
	}
	merged := make(map[string]bool, len(src)+1)
	for k, v := range src {
		merged[k] = v
	}
	merged[login] = true
	return &Processor{
		Client:         client,
		DryRun:         dryRun,
		MergeAutoMerge: mergeAutoMerge,
		Login:          login,
		TrustedAuthors: merged,
	}
}

func (p *Processor) ProcessPR(ctx context.Context, info pr.PRInfo, status *pr.PRStatus, idx int) {
	pullReq, _, err := p.Client.PullRequests.Get(ctx, info.Owner, info.Repo, info.Number)
	if err != nil {
		status.Update(idx, pr.StatusFailed, ghErrorDetail("fetch error", err))
		return
	}

	// On any failure outcome, look for a prior automated rescue attempt
	// recorded on the PR (an ai-rescue marker comment) so the operator can
	// tell "needs a first rescue" apart from "a rescue already failed here".
	// Deferred so every failure path is covered with one call site.
	defer func() {
		p.attachRescueMarker(ctx, info, pullReq, status, idx)
	}()

	actualAuthor := pullReq.GetUser().GetLogin()
	if !p.isAuthorTrusted(actualAuthor) {
		status.Update(idx, pr.StatusUntrustedAuthor, fmt.Sprintf("author %q not trusted", actualAuthor))
		return
	}

	if pullReq.GetMerged() {
		status.Update(idx, pr.StatusAlreadyMerged, "")
		return
	}

	if pullReq.GetMergeableState() == "dirty" {
		status.Update(idx, pr.StatusConflict, "merge conflict")
		return
	}

	status.Update(idx, pr.StatusChecking, "")
	if err := p.waitForChecks(ctx, info, pullReq, status, idx); err != nil {
		return
	}

	selfAuthored := strings.EqualFold(info.Author, p.Login)

	if p.DryRun {
		detail := "dry-run"
		if !selfAuthored {
			if err := p.ensureWriteAccess(ctx, info.Owner, info.Repo); err != nil {
				detail = withNote(detail, writeAccessDetail(err))
			}
		}
		status.Update(idx, pr.StatusSkipped, detail)
		return
	}

	if !selfAuthored {
		if err := p.approve(ctx, info, status, idx); err != nil {
			return
		}
	}

	if pullReq.GetAutoMerge() != nil && !p.MergeAutoMerge {
		status.Update(idx, pr.StatusAutoMerge, "auto-merge enabled")
		return
	}

	p.merge(ctx, info, status, idx)
}

func (p *Processor) waitForChecks(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest, status *pr.PRStatus, idx int) error {
	deadline := time.After(checkPollTimeout)
	for {
		outcome, err := p.getCombinedCheckState(ctx, info)
		if err != nil {
			status.Update(idx, pr.StatusFailed, ghErrorDetail("check error", err))
			return err
		}

		switch outcome.state {
		case stateSuccess:
			return nil
		case "blocked":
			// CI never ran because a GitHub Actions budget / spending-limit
			// block prevented every job from starting. This is not a code
			// failure, so surface it under a distinct status and keep it out
			// of the rescue path.
			status.Update(idx, pr.StatusBlockedCI, blockedDetail(outcome.blockedChecks))
			return fmt.Errorf("ci unavailable: actions budget")
		case stateFailure, stateError:
			// A CircleCI build that CircleCI itself cancelled carries no
			// verdict on the code, so it is neither a failure nor stale.
			// Decided first: it rests on positive evidence about this very
			// build, where staleness is a heuristic.
			cancelled, note := p.classifyCancelled(ctx, pullReq, outcome)
			if cancelled != nil {
				p.handleCancelled(ctx, cancelled, status, idx)
				return fmt.Errorf("checks cancelled")
			}
			// A failure that is already fixed on the base branch is stale,
			// not real: the branch is behind and every failing check is
			// green on the base head. Decided before the security split so
			// a stale govulncheck/Trivy failure is refreshed like any other.
			if stale := p.classifyStale(ctx, info, pullReq, outcome.failedChecks); stale != nil {
				p.handleStale(ctx, info, pullReq, stale, status, idx)
				return fmt.Errorf("checks stale")
			}
			if name := classifySecurityFailure(outcome.failedChecks, p.securityPatterns()); name != "" {
				status.Update(idx, pr.StatusFailedSecurity, withNote(fmt.Sprintf("security check failed: %s", name), note))
			} else {
				status.Update(idx, pr.StatusFailed, withNote(failureDetail(outcome.failedChecks), note))
			}
			return fmt.Errorf("checks failed")
		}

		status.Update(idx, pr.StatusChecking, outcome.state)

		select {
		case <-ctx.Done():
			status.Update(idx, pr.StatusSkipped, "cancelled")
			return ctx.Err()
		case <-deadline:
			status.Update(idx, pr.StatusFailed, "checks timed out")
			return fmt.Errorf("checks timed out")
		case <-time.After(checkPollInterval):
		}
	}
}

// checkOutcome is the result of evaluating a PR's combined commit status and
// check runs. failedChecks holds genuine failures; blockedChecks holds checks
// that failed solely because a GitHub Actions budget block kept the job from
// starting. The two are tracked separately so a billing block is never
// reported as a real CI failure.
type checkOutcome struct {
	state string
	// sha is the commit the statuses and check runs belong to: the PR head
	// as GitHub resolved it for this poll.
	sha           string
	failedChecks  []string
	blockedChecks []string
	// statusTargets maps each failing commit-status context to its
	// target_url, so the CircleCI lookup can find the build behind it.
	// Check runs have no entry.
	statusTargets map[string]string
}

func (p *Processor) getCombinedCheckState(ctx context.Context, info pr.PRInfo) (checkOutcome, error) {
	combined, _, err := p.Client.Repositories.GetCombinedStatus(ctx, info.Owner, info.Repo, fmt.Sprintf("refs/pull/%d/head", info.Number), nil)
	if err != nil {
		return checkOutcome{}, err
	}

	combinedState := combined.GetState()

	// Also check check-runs (GitHub Actions use check runs, not commit statuses)
	checkRuns, _, err := p.Client.Checks.ListCheckRunsForRef(ctx, info.Owner, info.Repo, fmt.Sprintf("refs/pull/%d/head", info.Number), nil)
	if err != nil {
		return checkOutcome{}, err
	}

	if checkRuns.GetTotal() == 0 && len(combined.Statuses) == 0 {
		// No checks configured -- treat as success
		return checkOutcome{state: stateSuccess}, nil
	}

	var failedChecks []string
	var blockedChecks []string
	allComplete := true
	hasFailure := false
	for _, cr := range checkRuns.CheckRuns {
		if cr.GetStatus() != statusCompleted {
			allComplete = false
			continue
		}
		conclusion := cr.GetConclusion()
		if conclusion == stateFailure || conclusion == "startup_failure" || conclusion == "timed_out" || conclusion == "cancelled" {
			name := cr.GetName()
			// A job that never started because of an Actions budget /
			// spending-limit block is not a real failure -- route it to the
			// blocked bucket instead so it is surfaced separately and kept
			// out of the rescue path.
			if p.isBudgetBlockedCheckRun(ctx, info, cr) {
				if name != "" {
					blockedChecks = append(blockedChecks, name)
				}
				continue
			}
			hasFailure = true
			if name != "" {
				failedChecks = append(failedChecks, name)
			}
		}
	}

	var statusTargets map[string]string
	for _, s := range combined.Statuses {
		state := s.GetState()
		if state == stateFailure || state == stateError {
			if name := s.GetContext(); name != "" {
				failedChecks = append(failedChecks, name)
				if statusTargets == nil {
					statusTargets = make(map[string]string)
				}
				statusTargets[name] = s.GetTargetURL()
			}
		}
	}

	out := checkOutcome{
		sha:           combined.GetSHA(),
		failedChecks:  failedChecks,
		blockedChecks: blockedChecks,
		statusTargets: statusTargets,
	}
	switch {
	case hasFailure:
		out.state = stateFailure
	case !allComplete:
		out.state = statePending
	case combinedState == stateFailure || combinedState == stateError:
		out.state = combinedState
	case len(blockedChecks) > 0:
		// Every failing check was a budget block and nothing genuinely
		// failed: the PR's CI could not run at all.
		out.state = "blocked"
	case combinedState == statePending && len(combined.Statuses) > 0:
		out.state = statePending
	default:
		out.state = stateSuccess
	}
	return out, nil
}

// isBudgetBlockedCheckRun reports whether a failed check run failed only
// because a GitHub Actions budget / spending-limit block prevented the job
// from starting. It inspects the check run's output fields first (cheap, no
// extra request) and falls back to fetching the run's annotations, which is
// where GitHub records the "job was not started because an Actions budget is
// preventing further use" message. Annotation-fetch errors are treated as
// "not a budget block" so a transient API error never hides a real failure.
func (p *Processor) isBudgetBlockedCheckRun(ctx context.Context, info pr.PRInfo, cr *github.CheckRun) bool {
	out := cr.GetOutput()
	title := out.GetTitle()
	summary := out.GetSummary()
	text := out.GetText()
	if isBudgetBlockOutput(title, summary, text, nil) {
		return true
	}

	if out.GetAnnotationsCount() == 0 {
		return false
	}

	annotations, _, err := p.Client.Checks.ListCheckRunAnnotations(ctx, info.Owner, info.Repo, cr.GetID(), nil)
	if err != nil {
		return false
	}
	messages := make([]string, 0, len(annotations))
	for _, a := range annotations {
		messages = append(messages, a.GetMessage())
	}
	return isBudgetBlockOutput("", "", "", messages)
}

// attachRescueMarker looks for the newest ai-rescue marker in the PR's
// comments and attaches it to the status entry. It only runs for failure
// and stale outcomes (the marker is operator triage signal: "an automated
// rescue was already attempted here"), skips entries that already carry a
// marker (the stale-refresh path looks it up first), and degrades silently
// on API errors -- a comment-listing failure must never change a sweep
// result. Staleness is decided against the PR's current head and content
// (see markStale).
func (p *Processor) attachRescueMarker(ctx context.Context, info pr.PRInfo, pullReq *github.PullRequest, status *pr.PRStatus, idx int) {
	switch status.StateAt(idx) {
	case pr.StatusFailed, pr.StatusFailedSecurity, pr.StatusConflict, pr.StatusStale, pr.StatusRefreshed:
	default:
		return
	}
	if status.RescueAt(idx) != nil {
		return
	}

	marker := p.findRescueMarker(ctx, info)
	if marker == nil {
		return
	}
	p.markStale(ctx, info, pullReq, marker)
	status.SetRescue(idx, marker)
}

// findRescueMarker returns the newest ai-rescue marker among the PR's
// comments, or nil when there is none or the comments cannot be listed.
// Staleness is left to the caller (markStale against the PR it cares
// about).
func (p *Processor) findRescueMarker(ctx context.Context, info pr.PRInfo) *pr.RescueMarker {
	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var marker *pr.RescueMarker
	for {
		comments, resp, err := p.Client.Issues.ListComments(ctx, info.Owner, info.Repo, info.Number, opts)
		if err != nil {
			return nil
		}
		// Comments are returned oldest-first; the last marker found wins.
		for _, c := range comments {
			if m := pr.ParseRescueMarker(c.GetBody()); m != nil {
				marker = m
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return marker
}

// securityPatterns returns the normalized security-check pattern list to
// use for classification: nil SecurityCheckPatterns falls back to the
// built-in defaults; a non-nil empty slice disables classification.
func (p *Processor) securityPatterns() []string {
	if p.SecurityCheckPatterns == nil {
		return normalizePatterns(defaultSecurityCheckPatterns)
	}
	return normalizePatterns(p.SecurityCheckPatterns)
}

// failureDetail builds a human-readable detail string for a non-security
// check failure, naming the failing checks when available.
func failureDetail(failedChecks []string) string {
	if len(failedChecks) == 0 {
		return "checks failed"
	}
	const maxShow = 3
	if len(failedChecks) <= maxShow {
		return fmt.Sprintf("checks failed: %s", strings.Join(failedChecks, ", "))
	}
	return fmt.Sprintf("checks failed: %s (+%d more)", strings.Join(failedChecks[:maxShow], ", "), len(failedChecks)-maxShow)
}

// withNote appends an operator note (for instance why a CircleCI build could
// not be inspected) to a status detail. An empty note leaves the detail as
// is.
func withNote(detail, note string) string {
	if note == "" {
		return detail
	}
	return detail + "; " + note
}

func (p *Processor) approve(ctx context.Context, info pr.PRInfo, status *pr.PRStatus, idx int) error {
	reviews, _, err := p.Client.PullRequests.ListReviews(ctx, info.Owner, info.Repo, info.Number, nil)
	if err != nil {
		status.Update(idx, pr.StatusFailed, ghErrorDetail("review list error", err))
		return err
	}

	for _, r := range reviews {
		if r.GetUser().GetLogin() == p.Login && r.GetState() == "APPROVED" {
			return nil
		}
	}

	if err := p.ensureWriteAccess(ctx, info.Owner, info.Repo); err != nil {
		status.Update(idx, pr.StatusFailed, writeAccessDetail(err))
		return err
	}

	status.Update(idx, pr.StatusApproving, "")

	event := "APPROVE"
	_, _, err = p.Client.PullRequests.CreateReview(ctx, info.Owner, info.Repo, info.Number, &github.PullRequestReviewRequest{
		Event: &event,
	})
	if err != nil {
		status.Update(idx, pr.StatusFailed, ghErrorDetail("approve error", err))
		return err
	}

	return nil
}

// isBaseBranchModified returns true when the merge failed because the base
// branch SHA changed (e.g. another PR was just merged into the same branch).
// These errors are retryable -- re-fetching the PR and retrying usually works.
func isBaseBranchModified(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "base branch was modified")
}

func (p *Processor) mergeRetries() int {
	if p.MergeMaxRetries > 0 {
		return p.MergeMaxRetries
	}
	return mergeMaxRetries
}

func (p *Processor) mergeWait() time.Duration {
	if p.MergeRetryWait > 0 {
		return p.MergeRetryWait
	}
	return mergeRetryBaseWait
}

func (p *Processor) merge(ctx context.Context, info pr.PRInfo, status *pr.PRStatus, idx int) {
	maxRetries := p.mergeRetries()
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt == 1 {
			status.Update(idx, pr.StatusMerging, "")
		} else {
			status.Update(idx, pr.StatusRetrying, fmt.Sprintf("attempt %d/%d", attempt, maxRetries))
		}

		_, _, err := p.Client.PullRequests.Merge(ctx, info.Owner, info.Repo, info.Number, "", &github.PullRequestOptions{
			MergeMethod: "squash",
		})
		if err == nil {
			status.Update(idx, pr.StatusMerged, "squash")
			return
		}

		if !isBaseBranchModified(err) {
			// Permanent error -- do not retry.
			errMsg := err.Error()
			if strings.Contains(errMsg, "409") || strings.Contains(errMsg, "conflict") {
				status.Update(idx, pr.StatusConflict, "merge conflict")
			} else {
				status.Update(idx, pr.StatusFailed, ghErrorDetail("merge error", err))
			}
			return
		}

		if attempt == maxRetries {
			status.Update(idx, pr.StatusFailed, fmt.Sprintf("base branch modified after %d attempts", maxRetries))
			return
		}

		// Wait with linear backoff before retrying.
		wait := p.mergeWait() * time.Duration(attempt)
		select {
		case <-ctx.Done():
			status.Update(idx, pr.StatusSkipped, "cancelled")
			return
		case <-time.After(wait):
		}

		// Re-fetch the PR to confirm it is still open and mergeable.
		refreshed, _, fetchErr := p.Client.PullRequests.Get(ctx, info.Owner, info.Repo, info.Number)
		if fetchErr != nil {
			status.Update(idx, pr.StatusFailed, ghErrorDetail("retry fetch error", fetchErr))
			return
		}
		if refreshed.GetMerged() {
			status.Update(idx, pr.StatusAlreadyMerged, "merged between retries")
			return
		}
		if refreshed.GetMergeableState() == "dirty" {
			status.Update(idx, pr.StatusConflict, "merge conflict on retry")
			return
		}
	}
}

func (p *Processor) isAuthorTrusted(login string) bool {
	if strings.EqualFold(login, p.Login) {
		return true
	}
	return p.TrustedAuthors[login]
}

func ghErrorDetail(prefix string, err error) string {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) {
		for _, e := range ghErr.Errors {
			if e.Message != "" {
				return fmt.Sprintf("%s: %s", prefix, e.Message)
			}
		}
		if ghErr.Message != "" {
			return fmt.Sprintf("%s: %s", prefix, ghErr.Message)
		}
	}
	return fmt.Sprintf("%s: %v", prefix, err)
}
