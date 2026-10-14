package kube

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIsAlreadyExistsRecognisesTheAPIsRefusal. This is the whole reason Create
// exists beside Apply: `create` failing with AlreadyExists is the answer that
// lets a caller say "someone else got there, leave it alone", which an upsert can
// never produce. Reading it wrong turns a safe no-op into a hard failure, or —
// worse — a hard failure into a silent overwrite.
func TestIsAlreadyExistsRecognisesTheAPIsRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want bool
	}{
		"verbatim from the API": {`Error from server (AlreadyExists): secrets "openbao-unseal-key" already exists`, true},
		"lowercase variant":     {"error: secrets already exists", true},
		"a different refusal":   {`Error from server (Forbidden): secrets is forbidden`, false},
		"not found":             {`Error from server (NotFound): secrets "x" not found`, false},
		"empty":                 {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsAlreadyExists(tc.out); got != tc.want {
				t.Errorf("IsAlreadyExists(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestCreateFeedsTheManifestOnStdinAndReturnsOutput runs the real Create against
// a fake `kubectl` on PATH, so the argv and the stdin plumbing are exercised
// rather than described. Create's contract is specifically that it returns
// kubectl's COMBINED output — IsAlreadyExists above reads it, and a version that
// returned only stdout would classify every conflict as an unknown failure.
func TestCreateFeedsTheManifestOnStdinAndReturnsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh")
	}
	dir := t.TempDir()
	// Echoes its argv and its stdin to STDERR, so a Create that dropped
	// CombinedOutput would come back empty and fail the assertions below.
	script := "#!/bin/sh\necho \"argv: $*\" >&2\ncat >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := Create("kind: Secret\nmetadata:\n  name: openbao-unseal-key\n")
	if err == nil {
		t.Fatal("the fake kubectl exits 1; Create must surface that")
	}
	if !strings.Contains(out, "argv: create -f -") {
		t.Errorf("Create must invoke `kubectl create -f -`, got: %q", out)
	}
	if !strings.Contains(out, "name: openbao-unseal-key") {
		t.Errorf("the manifest must reach kubectl on stdin, got: %q", out)
	}
}

// TestCreateSucceedsWhenKubectlDoes pins the happy path — the seam must not
// invent an error when the API accepted the object.
func TestCreateSucceedsWhenKubectlDoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"),
		[]byte("#!/bin/sh\ncat >/dev/null\necho 'secret/openbao-unseal-key created'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := Create("kind: Secret\n")
	if err != nil {
		t.Fatalf("Create: %v (%s)", err, out)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("Create must return kubectl's output, got %q", out)
	}
	if IsAlreadyExists(out) {
		t.Error("a successful create must not read as a conflict")
	}
}
