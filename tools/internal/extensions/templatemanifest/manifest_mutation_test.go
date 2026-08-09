package templatemanifest

import (
	"bytes"
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

	_, err := Load(root)
	if err == nil {
		t.Fatal("a one-field rule must be rejected")
	}
	if !strings.Contains(err.Error(), ":5 bad rule") {
		t.Errorf("error must point at line 5 (comments and blanks still consume a line number), got: %v", err)
	}
}

// The reverse containment (ADR 0014) exempts copier's `_answers_file`, and that
// branch survived every test in the suite when deleted. It matters because copier
// REGENERATES the answers tracker itself rather than merging it, so listing it
// under `_skip_if_exists` is correct however the manifest classes it — the fence
// is not an ownership claim there. Without the exemption, a template that fences
// its own answers file (which copier.yml does today) fails the gate the moment the
// manifest calls that file anything but `owned`.
func TestReverseFencingExemptsTheCopierAnswersFile(t *testing.T) {
	root := t.TempDir()
	scaffold := filepath.Join(root, "instance-template")
	// classed `managed`, which would otherwise trip the reverse check
	writeTestFile(t, scaffold, ".template-manifest", "managed **\n")
	writeTestFile(t, scaffold, ".copier-answers.yml", "_commit: v1\n")
	writeTestFile(t, root, "copier.yml",
		"_answers_file: .copier-answers.yml\n_skip_if_exists:\n  - \".copier-answers.yml\"\n_exclude: []\n")

	var out, errOut bytes.Buffer
	if err := Run(scaffold, "", "", &out, &errOut); err != nil {
		t.Fatalf("the answers tracker is copier's own file, not an ownership claim: %v\nstderr: %s",
			err, errOut.String())
	}
}
