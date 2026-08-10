package templatemanifest

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// scaffold builds a repo whose scaffold sits under instance-template/ with a
// copier.yml at the REPO root — the real layout, and the one that matters here:
// checkCopierFencing reads copier.yml from the scaffold's PARENT.
func scaffold(t *testing.T, manifestBody, copierBody string) (root, sc string) {
	t.Helper()
	root = t.TempDir()
	sc = filepath.Join(root, "instance-template")
	if err := os.MkdirAll(sc, 0o755); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{
		filepath.Join(root, "copier.yml"):       copierBody,
		filepath.Join(sc, ".template-manifest"): manifestBody,
		filepath.Join(sc, "kept.txt"):           "x\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, sc
}

func runCmd(t *testing.T, args ...string) error {
	t.Helper()
	c := Cmd()
	c.SetArgs(args)
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	return c.Execute()
}

// THE COPIER CHECK MUST STILL REACH copier.yml ABOVE THE SCAFFOLD, and the
// FAILING case is what proves it. checkCopierFencing returns nil early when it
// cannot find a copier config — so a reader fenced at the scaffold, which would
// refuse ../copier.yml, makes this check silently pass rather than fail. Only a
// finding it can produce ONLY by reading the file distinguishes the two.
func TestTheCopierCheckReadsCopierYmlAboveTheScaffold(t *testing.T) {
	// An `owned` file that copier does NOT protect: findable only if copier.yml
	// was opened.
	_, sc := scaffold(t, "managed  *\nowned  kept.txt\n", "_skip_if_exists:\n  - something-else.txt\n")
	err := runCmd(t, "--root", sc)
	if err == nil {
		t.Fatal("an owned file absent from copier's protect list must fail — a pass here means " +
			"copier.yml was never read and the check quietly became a no-op")
	}
	if !strings.Contains(err.Error(), "copier") {
		t.Errorf("the error should name the copier fencing, got: %v", err)
	}

	// The same scaffold with the file properly protected passes.
	_, sc2 := scaffold(t, "managed  *\nowned  kept.txt\n", "_skip_if_exists:\n  - kept.txt\n")
	if err := runCmd(t, "--root", sc2); err != nil {
		t.Errorf("a properly fenced scaffold must pass: %v", err)
	}
}

// An UNCLASSIFIED scaffold file fails — the gate's whole purpose.
func TestCmdFailsOnAnUnclassifiedFile(t *testing.T) {
	_, sc := scaffold(t, "managed  never-matches-anything\n", "")
	err := runCmd(t, "--root", sc)
	if err == nil {
		t.Fatal("a scaffold file matching no rule must fail the gate")
	}
	if !strings.Contains(err.Error(), "unclassified") {
		t.Errorf("the error should name the unclassified files, got: %v", err)
	}
}

// AN EMPTY --root MEANS AUTO-DETECT and must stay empty. filepath.Clean("") is
// ".", so handing it to the fence helper would turn "look for instance-template/
// then ." into "look in .", scanning the working directory instead.
func TestAnEmptyRootStillAutoDetects(t *testing.T) {
	root, _ := scaffold(t, "managed  *\nowned  kept.txt\n", "_skip_if_exists:\n  - kept.txt\n")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	if err := runCmd(t); err != nil {
		t.Fatalf("auto-detect from the repo root must find instance-template/: %v", err)
	}
}

// --classify and --list are the query surface, and they go through the same
// fenced door.
func TestClassifyAndListQueryThroughTheFence(t *testing.T) {
	_, sc := scaffold(t, "managed  *\nowned  kept.txt\n", "_skip_if_exists:\n  - kept.txt\n")

	var out strings.Builder
	c := Cmd()
	c.SetArgs([]string{"--root", sc, "--classify", "kept.txt"})
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetOut(&out)
	c.SetErr(io.Discard)
	if err := c.Execute(); err != nil {
		t.Fatalf("--classify: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "owned" {
		t.Errorf("--classify kept.txt = %q, want owned", got)
	}

	if err := runCmd(t, "--root", sc, "--classify", "kept.txt", "--list", "owned"); err == nil {
		t.Error("--classify and --list together must be refused")
	}
}

// The reader comes from the DECLARATION, not from a binding the guard invented.
func TestGateBindingComesFromTheDeclaration(t *testing.T) {
	if b := gateBinding(); b.Kind != extension.Gate {
		t.Errorf("gateBinding returned a %s binding, want a gate", b.Kind)
	}
	if Extension().Name == "" {
		t.Error("the extension has no name")
	}
}
