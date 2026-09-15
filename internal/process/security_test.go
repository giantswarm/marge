package process

import "testing"

func TestClassifySecurityFailure_defaults(t *testing.T) {
	defaults := normalizePatterns(DefaultSecurityCheckPatterns())

	tests := []struct {
		name          string
		failedChecks  []string
		wantNonEmpty  bool
		wantMatchedIs string
	}{
		{
			name:          "govulncheck check matches",
			failedChecks:  []string{"build", "govulncheck"},
			wantNonEmpty:  true,
			wantMatchedIs: "govulncheck",
		},
		{
			name:          "Trivy check matches case-insensitively",
			failedChecks:  []string{"TRIVY: container scan"},
			wantNonEmpty:  true,
			wantMatchedIs: "TRIVY: container scan",
		},
		{
			name:          "CodeQL Analysis matches",
			failedChecks:  []string{"CodeQL Analysis (go)"},
			wantNonEmpty:  true,
			wantMatchedIs: "CodeQL Analysis (go)",
		},
		{
			name:          "Security Scan matches",
			failedChecks:  []string{"Security Scan"},
			wantNonEmpty:  true,
			wantMatchedIs: "Security Scan",
		},
		{
			name:         "build/test failure does not match",
			failedChecks: []string{"build", "unit-tests", "lint"},
			wantNonEmpty: false,
		},
		{
			name:         "no failed checks returns empty",
			failedChecks: nil,
			wantNonEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySecurityFailure(tt.failedChecks, defaults)
			if tt.wantNonEmpty && got == "" {
				t.Errorf("classifySecurityFailure(%v) returned empty, want non-empty", tt.failedChecks)
			}
			if !tt.wantNonEmpty && got != "" {
				t.Errorf("classifySecurityFailure(%v) = %q, want empty", tt.failedChecks, got)
			}
			if tt.wantNonEmpty && tt.wantMatchedIs != "" && got != tt.wantMatchedIs {
				t.Errorf("classifySecurityFailure(%v) = %q, want %q", tt.failedChecks, got, tt.wantMatchedIs)
			}
		})
	}
}

func TestClassifySecurityFailure_customPatterns(t *testing.T) {
	patterns := normalizePatterns([]string{"my-custom-scan"})

	if got := classifySecurityFailure([]string{"govulncheck"}, patterns); got != "" {
		t.Errorf("govulncheck should not match custom patterns; got %q", got)
	}

	if got := classifySecurityFailure([]string{"My-Custom-Scan failed"}, patterns); got == "" {
		t.Errorf("expected custom pattern to match case-insensitively")
	}
}

func TestClassifySecurityFailure_emptyPatternsDisableClassification(t *testing.T) {
	if got := classifySecurityFailure([]string{"govulncheck"}, []string{}); got != "" {
		t.Errorf("empty patterns should disable classification; got %q", got)
	}
}

func TestNormalizePatterns_dropsBlankAndNormalizesCase(t *testing.T) {
	got := normalizePatterns([]string{"", "  ", "Trivy", " GoVulnCheck "})
	want := []string{"trivy", "govulncheck"}
	if len(got) != len(want) {
		t.Fatalf("normalizePatterns len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("normalizePatterns[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDefaultSecurityCheckPatterns_returnsCopy(t *testing.T) {
	a := DefaultSecurityCheckPatterns()
	if len(a) == 0 {
		t.Fatal("expected non-empty default patterns")
	}
	a[0] = "MUTATED"

	b := DefaultSecurityCheckPatterns()
	if b[0] == "MUTATED" {
		t.Fatal("DefaultSecurityCheckPatterns must return an independent copy")
	}
}

func TestProcessorSecurityPatterns_defaultWhenNil(t *testing.T) {
	p := &Processor{}
	if got := p.securityPatterns(); len(got) == 0 {
		t.Fatal("expected default security patterns when SecurityCheckPatterns is nil")
	}
}

// TestProcessorSecurityPatterns_extraOnlyWidens guards that the security
// guard cannot be narrowed: the caller's patterns are added to the built-in
// list, and an empty list leaves the built-in list intact.
func TestProcessorSecurityPatterns_extraOnlyWidens(t *testing.T) {
	defaults := len((&Processor{}).securityPatterns())

	p := &Processor{SecurityCheckPatterns: []string{}}
	if got := p.securityPatterns(); len(got) != defaults {
		t.Fatalf("empty extra list: got %d patterns, want the %d built-in ones", len(got), defaults)
	}

	p = &Processor{SecurityCheckPatterns: []string{"Analyze"}}
	got := p.securityPatterns()
	if len(got) != defaults+1 {
		t.Fatalf("one extra pattern: got %d patterns, want %d", len(got), defaults+1)
	}
	if classifySecurityFailure([]string{"Analyze (go)"}, got) == "" {
		t.Fatal("the extra pattern must classify a matching check as security")
	}
	if classifySecurityFailure([]string{"govulncheck"}, got) == "" {
		t.Fatal("the built-in patterns must still apply")
	}
}

// TestClassifySecurityFailure_realWorldNames feeds actual check-run names
// observed in production GitHub Actions workflows through the classifier.
// Names captured from live API responses on 2026-05-18:
//   - aquasecurity/trivy:    "Scan Go vulnerabilities"
//   - golang/vuln (CodeQL):  "Analyze (go)"
//   - teemow/inboxfewer:     "gitleaks", "Lint and Test", "build-and-push"
//   - teemow/stammbaum:      "backend"
func TestClassifySecurityFailure_realWorldNames(t *testing.T) {
	defaults := normalizePatterns(DefaultSecurityCheckPatterns())

	tests := []struct {
		name         string
		checkName    string
		wantSecurity bool
	}{
		{"aquasecurity trivy scan", "Scan Go vulnerabilities", true},
		{"gitleaks default", "gitleaks", true},
		{"CodeQL default job name", "Analyze (go)", false}, // documents the known gap
		{"build failure", "Lint and Test", false},
		{"docker build", "build-and-push", false},
		{"generic backend job", "backend", false},
		{"go test job", "Test (macos-latest)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifySecurityFailure([]string{tt.checkName}, defaults)
			if tt.wantSecurity && got == "" {
				t.Errorf("expected %q to be classified as security, but it was not", tt.checkName)
			}
			if !tt.wantSecurity && got != "" {
				t.Errorf("expected %q NOT to be classified as security, but it was (matched as %q)", tt.checkName, got)
			}
		})
	}
}

func TestFailureDetail(t *testing.T) {
	tests := []struct {
		name         string
		failedChecks []string
		want         string
	}{
		{"no checks", nil, "checks failed"},
		{"one check", []string{"build"}, "checks failed: build"},
		{"three checks", []string{"a", "b", "c"}, "checks failed: a, b, c"},
		{"more than three", []string{"a", "b", "c", "d", "e"}, "checks failed: a, b, c (+2 more)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failureDetail(tt.failedChecks); got != tt.want {
				t.Errorf("failureDetail(%v) = %q, want %q", tt.failedChecks, got, tt.want)
			}
		})
	}
}
