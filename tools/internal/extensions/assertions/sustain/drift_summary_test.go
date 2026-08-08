package sustain

// drift_summary_test.go — emitDriftSummary moved here with the drift verb. It was
// in package main's coverage_tier2_test.go, another grab-bag: the extraction keeps
// finding tests that lived where there was room rather than where they belonged.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmitDriftSummary(t *testing.T) {
	summary := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summary, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	tv := TemplateVersion{TemplateRepo: "akamai/llz", StampedAt: "2026-01-01", TemplateRef: "v1.0.0", TemplateSHA: "abcdef1234567890"}
	emitDriftSummary(tv, "main", "deadbeefcafe", "3", "behind")

	b, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"Template drift — akamai/llz", "v1.0.0", "abcdef12", "| Commits behind | 3 |", "| Status | behind |"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}

	// behind == "" omits the row; no GITHUB_STEP_SUMMARY is a no-op (no panic).
	emitDriftSummary(tv, "main", "deadbeefcafe", "", "up to date")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	emitDriftSummary(tv, "main", "deadbeefcafe", "1", "behind")
}
