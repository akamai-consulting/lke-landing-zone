package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCosignSubjectGuardNamesFilesRepoRelative pins the path form in the
// failure message. The guard runs with --root pointed at the checkout, and the
// operator's next move is to open the offending file — a container-absolute
// path (/github/workspace/…) or a temp-dir path does not exist on their
// machine, so the annotation has to be repo-relative to be actionable.
func TestCosignSubjectGuardNamesFilesRepoRelative(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "platform-apl", "manifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := "verifyImages:\n" +
		"  - attestors:\n" +
		"      - entries:\n" +
		"          - keyless:\n" +
		"              subject: \"https://github.com/o/r/.github/workflows/renamed-away.yml@*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runCosignSubjectGuard(root)
	if err == nil {
		t.Fatal("guard passed while its pinned workflow was absent")
	}
	rel := filepath.ToSlash(filepath.Join("platform-apl", "manifest", "policy.yaml"))
	if !strings.Contains(err.Error(), rel) {
		t.Errorf("error must locate the file as %q:\n%v", rel, err)
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("error must NOT carry the absolute scan root %q (it does not exist on the operator's machine):\n%v", root, err)
	}
}
