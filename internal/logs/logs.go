// Package logs reads a bounded excerpt of a failing check's output. A rule
// matches its signal against that excerpt, never against a check name alone.
package logs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/circleci"
)

// downloadLimit caps what one excerpt reads from the network, whatever the
// rule asked for. A job log can be hundreds of megabytes.
const downloadLimit = 8 << 20

// Fetcher reads check logs. A source with no client returns no excerpt, and
// a rule that needs it does not match.
type Fetcher struct {
	GitHub   *github.Client
	CircleCI *circleci.Client
	// HTTP downloads the redirect target of an Actions job log. Nil uses a
	// client with a timeout of its own.
	HTTP *http.Client
}

// jobURLRE reads the job id out of a check run's details URL, which is the
// documented link from a check run to the Actions job behind it.
var jobURLRE = regexp.MustCompile(`/actions/runs/\d+/job/(\d+)`)

// Actions returns the tail of one GitHub Actions job log. detailsURL is the
// check run's details URL.
func (f *Fetcher) Actions(ctx context.Context, owner, repo, detailsURL string, maxBytes int) (string, bool) {
	if f == nil || f.GitHub == nil {
		return "", false
	}
	match := jobURLRE.FindStringSubmatch(detailsURL)
	if match == nil {
		return "", false
	}
	jobID, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return "", false
	}
	logURL, _, err := f.GitHub.Actions.GetWorkflowJobLogs(ctx, owner, repo, jobID, 1)
	if err != nil || logURL == nil {
		return "", false
	}
	excerpt, err := f.download(ctx, logURL.String(), maxBytes)
	if err != nil {
		return "", false
	}
	return excerpt, true
}

// CircleCIBuild returns the output of the failing actions of a CircleCI
// build, newest step last, bounded by maxBytes.
func (f *Fetcher) CircleCIBuild(ctx context.Context, targetURL string, maxBytes int) (string, bool) {
	if f == nil || f.CircleCI == nil {
		return "", false
	}
	ref, ok := circleci.ParseBuildURL(targetURL)
	if !ok {
		return "", false
	}
	build, err := f.CircleCI.Build(ctx, ref)
	if err != nil {
		return "", false
	}

	var b strings.Builder
	for _, step := range build.Steps {
		for _, action := range step.Actions {
			if !action.Failed && action.Status != "failed" && !action.Canceled {
				continue
			}
			lines, err := f.CircleCI.StepOutput(ctx, ref, action)
			if err != nil || len(lines) == 0 {
				continue
			}
			fmt.Fprintf(&b, "==> %s\n", step.Name)
			for _, line := range lines {
				b.WriteString(line.Message)
			}
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return lastBytes(b.String(), maxBytes), true
}

func (f *Fetcher) download(ctx context.Context, url string, maxBytes int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading log: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, downloadLimit))
	if err != nil {
		return "", err
	}
	return aroundError(string(body), maxBytes), nil
}

// errorMarker is what the Actions runner writes in front of the line that
// failed a step.
const errorMarker = "##[error]"

// aroundError returns the excerpt a rule matches against: the maxBytes that
// end just after the last failure the runner reported. A job log ends with
// credential cleanup and orphan-process removal, so its tail says nothing
// about why the job failed. A log with no error marker falls back to the
// tail, which is where a crash without a marker leaves its output.
func aroundError(body string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	end := len(body)
	if at := strings.LastIndex(body, errorMarker); at >= 0 {
		end = at + len(errorMarker)
		if nl := strings.IndexByte(body[end:], '\n'); nl >= 0 {
			end += nl + 1
		} else {
			end = len(body)
		}
	}
	start := end - maxBytes
	if start < 0 {
		start = 0
	}
	return body[start:end]
}

// tail returns the last maxBytes of a stream without holding more than that
// in memory.
func tail(r io.Reader, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		return "", nil
	}
	buf := make([]byte, 0, maxBytes)
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > maxBytes {
				buf = buf[len(buf)-maxBytes:]
			}
		}
		if err == io.EOF {
			return string(buf), nil
		}
		if err != nil {
			return "", err
		}
	}
}

func lastBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return s[len(s)-maxBytes:]
}
