package lint

// pin_coherence_step_test.go — the wrapper test, now beside the wrapper.
//
// The guard itself is internal/extensions/pincoherence and its tests went with
// it. This one tests the LINT STEP that calls it — that the step reads the WORKING
// DIRECTORY rather than a path it was handed. It followed checks.go here.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
)

func TestStepPinCoherenceUsesWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("_commit: v0.0.33\nllz_version: v0.0.34\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := stepPinCoherence(cliopts.Opts{}); err == nil {
		t.Fatal("stepPinCoherence() should fail on a skewed pin in the working directory")
	}
}
