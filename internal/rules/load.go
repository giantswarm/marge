package rules

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/giantswarm/marge/internal/remedy"
)

// The catalogue lives in marge's own repository and is read from its default
// branch at the start of every sweep, so a merged rule is live on the next
// run without a release.
const (
	DefaultOwner = "giantswarm"
	DefaultRepo  = "marge"
	DefaultDir   = "rules"
	DefaultRef   = "main"
)

// Loader reads the catalogue from a repository, or from LocalPath when that
// is set.
type Loader struct {
	Client *github.Client
	Owner  string
	Repo   string
	// Dir is the directory inside the repository that holds the documents.
	Dir string
	Ref string

	// LocalPath reads the catalogue from this directory instead of GitHub.
	LocalPath string
}

// Catalogue is the loaded rule set of one sweep.
type Catalogue struct {
	// Rules are ordered by name. The first rule that matches a PR wins, so
	// the order is part of the contract and a scenario test pins it.
	Rules []*Rule
	// Digest identifies the exact catalogue this sweep ran, and is written
	// into the evidence of every action a rule selected.
	Digest string
	// Source says where the catalogue came from.
	Source string
	// Ref is the branch the catalogue was read from, empty for a local
	// directory.
	Ref string
	// Skipped names the documents that failed to parse, with the reason.
	// They are left out; the rest of the catalogue still runs.
	Skipped []Skipped
}

// Skipped is one document the catalogue could not use.
type Skipped struct {
	Path   string
	Reason string
}

// Available reports whether the sweep may run remedies. A catalogue that
// could not be read at all is not available and every remedy is refused.
func (c *Catalogue) Available() bool { return c != nil }

// Load reads and validates every document of the catalogue. A document that
// fails to parse is skipped and reported; a catalogue that cannot be read at
// all is an error, and the caller runs the sweep without remedies.
func (l Loader) Load(ctx context.Context, reg *remedy.Registry) (*Catalogue, error) {
	files, source, err := l.files(ctx)
	if err != nil {
		return nil, err
	}

	cat := &Catalogue{Source: source, Ref: l.effectiveRef(), Digest: digest(files)}
	for _, f := range files {
		rule, err := Parse(f.path, f.content, reg)
		if err != nil {
			cat.Skipped = append(cat.Skipped, Skipped{Path: f.path, Reason: err.Error()})
			continue
		}
		cat.Rules = append(cat.Rules, rule)
	}
	if err := cat.checkNames(); err != nil {
		return nil, err
	}
	return cat, nil
}

// checkNames refuses a catalogue with two rules of the same name: the name
// identifies a rule in every report and in every scenario test.
func (c *Catalogue) checkNames() error {
	seen := make(map[string]bool, len(c.Rules))
	for _, r := range c.Rules {
		if seen[r.Name] {
			return fmt.Errorf("rule %q is defined twice", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// file is one document as read, before it is parsed.
type file struct {
	path    string
	sha     string
	content []byte
}

// effectiveRef is the branch a repository catalogue is read from. A local
// directory has none.
func (l Loader) effectiveRef() string {
	if l.LocalPath != "" {
		return ""
	}
	if l.Ref == "" {
		return DefaultRef
	}
	return l.Ref
}

func (l Loader) files(ctx context.Context) ([]file, string, error) {
	if l.LocalPath != "" {
		return l.localFiles()
	}
	return l.repoFiles(ctx)
}

func (l Loader) localFiles() ([]file, string, error) {
	entries, err := os.ReadDir(l.LocalPath)
	if err != nil {
		return nil, "", fmt.Errorf("reading rule directory: %w", err)
	}
	var files []file
	for _, e := range entries {
		if e.IsDir() || !isRuleFile(e.Name()) {
			continue
		}
		full := filepath.Join(l.LocalPath, e.Name())
		content, err := os.ReadFile(full)
		if err != nil {
			return nil, "", fmt.Errorf("reading %s: %w", full, err)
		}
		files = append(files, file{path: e.Name(), sha: contentSHA(content), content: content})
	}
	sortFiles(files)
	return files, l.LocalPath, nil
}

func (l Loader) repoFiles(ctx context.Context) ([]file, string, error) {
	owner, repo, dir, ref := l.Owner, l.Repo, l.Dir, l.Ref
	if owner == "" {
		owner = DefaultOwner
	}
	if repo == "" {
		repo = DefaultRepo
	}
	if dir == "" {
		dir = DefaultDir
	}
	if ref == "" {
		ref = DefaultRef
	}
	source := fmt.Sprintf("%s/%s@%s:%s", owner, repo, ref, dir)

	opts := &github.RepositoryContentGetOptions{Ref: ref}
	_, entries, resp, err := l.Client.Repositories.GetContents(ctx, owner, repo, dir, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, source, fmt.Errorf("no rule directory at %s", source)
		}
		return nil, source, fmt.Errorf("listing %s: %w", source, err)
	}

	var files []file
	for _, e := range entries {
		if e.GetType() != "file" || !isRuleFile(e.GetName()) {
			continue
		}
		blob, _, err := l.Client.Git.GetBlob(ctx, owner, repo, e.GetSHA())
		if err != nil {
			return nil, source, fmt.Errorf("reading %s: %w", e.GetPath(), err)
		}
		content, err := decodeBlob(blob)
		if err != nil {
			return nil, source, fmt.Errorf("decoding %s: %w", e.GetPath(), err)
		}
		files = append(files, file{path: e.GetName(), sha: e.GetSHA(), content: content})
	}
	sortFiles(files)
	return files, source, nil
}

// decodeBlob returns a blob's bytes. The Git API answers base64 for every
// blob it did not serve as UTF-8.
func decodeBlob(blob *github.Blob) ([]byte, error) {
	switch encoding := blob.GetEncoding(); encoding {
	case "base64":
		return base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.GetContent(), "\n", ""))
	case "utf-8", "":
		return []byte(blob.GetContent()), nil
	default:
		return nil, fmt.Errorf("unsupported blob encoding %q", encoding)
	}
}

func isRuleFile(name string) bool {
	ext := path.Ext(name)
	return (ext == ".yaml" || ext == ".yml") && !strings.HasPrefix(name, ".")
}

func sortFiles(files []file) {
	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.path, b.path) })
}

// digest identifies a catalogue by the names and blob SHAs it is made of, so
// two sweeps that ran the same rules report the same value.
func digest(files []file) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.path + " " + f.sha + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func contentSHA(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
