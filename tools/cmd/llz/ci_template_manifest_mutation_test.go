package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadTemplateManifestLocatesTheBadRule pins the line number in the parse
// error. .template-manifest is hand-edited during upgrades and a bad rule aborts
// the whole classification, so "<path>:<n> bad rule" is the operator's only
// pointer into a file that is mostly globs — a number that does not match the
// editor's gutter sends them to the wrong line, or nowhere at all.
func TestLoadTemplateManifestLocatesTheBadRule(t *testing.T) {
	root := t.TempDir()
	body := "# a comment\n" + // line 1
		"managed **\n" + // line 2
		"\n" + // line 3 (blank)
		"merge docs/**\n" + // line 4
		"owned\n" // line 5 — one field, not two
	if err := os.WriteFile(filepath.Join(root, ".template-manifest"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadTemplateManifest(root)
	if err == nil {
		t.Fatal("a one-field rule must be rejected")
	}
	if !strings.Contains(err.Error(), ":5 bad rule") {
		t.Errorf("error must point at line 5 (comments and blanks still consume a line number), got: %v", err)
	}
}
