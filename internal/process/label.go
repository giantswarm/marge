package process

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/pr"
)

// labelColor is the colour of every classification label the sweep creates.
const labelColor = "0e8a16"

// labelCache remembers, per repository, that a label was already created
// in this run so a repository with many PRs pays the create once.
type labelCache struct {
	labelMu sync.Mutex
	labels  map[string]bool
}

// setLabel leaves exactly one marge/<class> label on the PR. Every
// error degrades to a note on the entry: a label is display only and may
// never change a sweep outcome.
func (p *Processor) setLabel(ctx context.Context, run *prRun, class string) {
	want := pr.LabelPrefix + class
	present := false
	for _, l := range run.pull.Labels {
		name := l.GetName()
		switch {
		case name == want:
			present = true
		case strings.HasPrefix(name, pr.LabelPrefix), strings.HasPrefix(name, pr.LegacyLabelPrefix):
			// go-github does not escape the label in the path; a slash in
			// the name would otherwise split the route.
			if _, err := p.Client.Issues.RemoveLabelForIssue(ctx, run.info.Owner, run.info.Repo, run.info.Number, url.PathEscape(name)); err != nil {
				run.note("label " + name + " not removed: " + ghErrorDetail("", err))
			}
		}
	}
	if present {
		run.status.SetLabel(run.idx, want)
		return
	}

	_, resp, err := p.Client.Issues.AddLabelsToIssue(ctx, run.info.Owner, run.info.Repo, run.info.Number, []string{want})
	if err == nil {
		run.status.SetLabel(run.idx, want)
		return
	}
	if !isStatus(err, resp, http.StatusNotFound) && !isStatus(err, resp, http.StatusUnprocessableEntity) {
		run.note("label not set: " + ghErrorDetail("", err))
		return
	}
	if err := p.ensureLabel(ctx, run.info, want); err != nil {
		run.note("label not created: " + ghErrorDetail("", err))
		return
	}
	if _, _, err := p.Client.Issues.AddLabelsToIssue(ctx, run.info.Owner, run.info.Repo, run.info.Number, []string{want}); err != nil {
		run.note("label not set: " + ghErrorDetail("", err))
		return
	}
	run.status.SetLabel(run.idx, want)
}

func (p *Processor) ensureLabel(ctx context.Context, info pr.PRInfo, name string) error {
	key := info.Owner + "/" + info.Repo + ":" + name
	p.labelMu.Lock()
	if p.labels == nil {
		p.labels = make(map[string]bool)
	}
	done := p.labels[key]
	p.labelMu.Unlock()
	if done {
		return nil
	}
	_, resp, err := p.Client.Issues.CreateLabel(ctx, info.Owner, info.Repo, github.CreateIssueLabelRequest{
		Name:        name,
		Color:       new(labelColor),
		Description: new("Classification written by the marge sweep; display only"),
	})
	if err != nil && !isStatus(err, resp, http.StatusUnprocessableEntity) {
		return err
	}
	p.labelMu.Lock()
	p.labels[key] = true
	p.labelMu.Unlock()
	return nil
}
