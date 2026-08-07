package main

// pin_coherence_step_test.go — the one pin-coherence test that stayed.
//
// The guard itself is internal/pincoherence and its tests went with it. This one
// did not, because its subject is main's wrapper: that the lint step reads the
// WORKING DIRECTORY rather than a path it was handed. That is a fact about how
// checks.go wires the gate, not about what the gate decides.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStepPinCoherenceUsesWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("_commit: v0.0.33\nllz_version: v0.0.34\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := stepPinCoherence(globalOpts{}); err == nil {
		t.Fatal("stepPinCoherence() should fail on a skewed pin in the working directory")
	}
}
