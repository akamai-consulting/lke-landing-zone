package templatemanifest

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
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

// TestTokenFreeCallerStubsMatchThisList pins the enumeration in
// .template-manifest of caller stubs that carry no copier token.
//
// IT IS A HAND-KEPT LIST THAT DECIDES SOMETHING. Those stubs are `merge`, so the
// managed-fresh digest gate does not cover them and a hand-edit there survives an
// upgrade silently; the prose is the only record of which files are in that
// position, and the sentence after it uses the list to reason about which could
// move to `managed`. It had already drifted — template-upgrade.yml arrived
// token-free while the prose still said "those three" — which is the whole
// argument for measuring it rather than re-reading it.
func TestTokenFreeCallerStubsMatchThisList(t *testing.T) {
	const scaffold = "../../../../../instance-template"
	entries, err := os.ReadDir(filepath.Join(scaffold, ".github/workflows"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		// llz-*.yml are the reusable BODIES (`managed`, digest-locked). promote.yml is
		// GENERATED and `owned`. Neither is a caller stub held in `merge`.
		if e.IsDir() || strings.HasPrefix(name, "llz-") || name == "promote.yml" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(scaffold, ".github/workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "<@") {
			got[name] = true
		}
	}
	if len(got) == 0 {
		t.Fatal("found no caller stubs at all — the scan examined nothing, which reads exactly like agreement")
	}

	manifest, err := os.ReadFile(filepath.Join(scaffold, ".template-manifest"))
	if err != nil {
		t.Fatal(err)
	}
	// SCOPED TO THE ENUMERATION, not to the file. Searching the whole manifest let
	// this test pass on the PARAGRAPH EXPLAINING the drift — which names
	// template-upgrade.yml — while the list itself still said "those three". A gate
	// that can be satisfied by prose about the gate is satisfied by nothing.
	// TOKENISED, NOT SUBSTRING-MATCHED. `strings.Contains` let a token-free
	// `upgrade.yml` false-pass off `template-upgrade.yml` — and `health.yml`,
	// `checks.yml` and `gameday.yml` off the other three — so a new stub could be
	// omitted from the list and still be reported as named.
	// BOTH DIRECTIONS. Checking only that every stub is named let the enumeration
	// OVER-state: give cluster-health.yml a copier token and it drops out of the
	// scan while the sentence goes on calling it token-free — and the manifest
	// claims this test pins that list, so a one-way check makes the claim false in
	// the direction nobody is looking.
	enum := namesIn(enumerationOfTokenFreeStubs(t, string(manifest)))
	for name := range got {
		if !enum[name] {
			t.Errorf("%s is a token-free `merge` caller stub but .template-manifest does not name it. That "+
				"list is the only record of which files sit OUTSIDE the managed-fresh digest gate, and the "+
				"sentence after it decides from the list which stubs could move to `managed`.", name)
		}
	}
	for name := range enum {
		if !got[name] {
			t.Errorf(".template-manifest lists %s as a token-free caller stub, but it is not one — it "+
				"either carries a copier token now or no longer exists. The list over-states, which is the "+
				"same staleness in the direction the membership check does not look.", name)
		}
	}
}

// enumerationOfTokenFreeStubs returns just the sentence in .template-manifest that
// lists the token-free caller stubs — from "and to ZERO for" to the end of that
// sentence. Fails the test if the anchor is gone, because a search over an empty
// string finds nothing and reads exactly like agreement.
func enumerationOfTokenFreeStubs(t *testing.T, manifest string) string {
	t.Helper()
	const anchor = "and to ZERO for"
	i := strings.Index(manifest, anchor)
	if i < 0 {
		t.Fatalf(".template-manifest no longer contains %q — this test can no longer find the list it "+
			"pins, and would otherwise pass having read nothing", anchor)
	}
	rest := manifest[i+len(anchor):]
	j := strings.Index(rest, ".")
	for j >= 0 && j+1 < len(rest) && rest[j+1] != ' ' && rest[j+1] != '\n' {
		// A period inside a filename (cluster-health.yml) is not the end of the
		// sentence; only one followed by a space or a newline is.
		k := strings.Index(rest[j+1:], ".")
		if k < 0 {
			break
		}
		j += 1 + k
	}
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// namesIn is the set of workflow filenames a sentence actually names.
func namesIn(sentence string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`[A-Za-z0-9._-]+\.yml`).FindAllString(sentence, -1) {
		out[m] = true
	}
	return out
}
