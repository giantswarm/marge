// Package docs holds the GitHub App record and the manifest that registers
// the App, and one test that keeps the two in step.
package docs

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

type manifest struct {
	DefaultPermissions map[string]string `json:"default_permissions"`
}

// rowPattern matches one body row of the permission table in github-app.md:
// "| Pull requests | `pull_requests` | write | ... |". The Metadata row has an
// empty key cell and does not match.
var rowPattern = regexp.MustCompile("(?m)^\\| [^|]+ \\| `([a-z_]+)` \\| (read|write) \\|")

func TestManifestMatchesTheRecord(t *testing.T) {
	raw, err := os.ReadFile("github-app-manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	record, err := os.ReadFile("github-app.md")
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	documented := make(map[string]string)
	for _, row := range rowPattern.FindAllStringSubmatch(string(record), -1) {
		documented[row[1]] = row[2]
	}
	if len(documented) == 0 {
		t.Fatal("no permission rows found in github-app.md; the table shape changed")
	}

	for key, level := range m.DefaultPermissions {
		got, ok := documented[key]
		if !ok {
			t.Errorf("manifest grants %q, and github-app.md gives no reason for it", key)
			continue
		}
		if got != level {
			t.Errorf("%q is %q in the manifest and %q in github-app.md", key, level, got)
		}
	}
	for key := range documented {
		if _, ok := m.DefaultPermissions[key]; !ok {
			t.Errorf("github-app.md documents %q, and the manifest does not grant it", key)
		}
	}

	// GitHub grants metadata to every App. Naming it in the manifest is a
	// registration error, not a permission.
	if _, ok := m.DefaultPermissions["metadata"]; ok {
		t.Error("the manifest names metadata; GitHub grants it implicitly")
	}
}

// TestRecordKeepsTheContentsWriteHazard guards the one sentence a
// least-privilege review must read before it trims a permission.
func TestRecordKeepsTheContentsWriteHazard(t *testing.T) {
	record, err := os.ReadFile("github-app.md")
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !strings.Contains(string(record), "Do not remove `Contents: write`") {
		t.Error("github-app.md lost the Contents: write hazard")
	}
}
