package main

import (
	"os"
	"path/filepath"
	"testing"
)

// execArgv's stdin plumbing is what keeps a secret VALUE out of the process
// arguments (`gh secret set` reads it from stdin). Nothing exercised it, so the
// guard could invert and quietly hand the child the parent's stdin instead —
// which in CI is /dev/null, i.e. an empty secret written with a zero exit code.
func TestExecArgvPipesStdinToTheChild(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "captured")
	if err := execArgv([]string{"sh", "-c", "cat > " + out}, "piped-secret-value\n"); err != nil {
		t.Fatalf("execArgv: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading what the child saw: %v", err)
	}
	if string(b) != "piped-secret-value\n" {
		t.Errorf("child stdin = %q, want the piped value", b)
	}
}
