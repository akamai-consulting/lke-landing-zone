package setupgosite

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write materialises one file under root, creating parents.
func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// composite is a stand-in for the real setup-llz action: the authority the gate
// measures every other file against.
const composite = `name: Set up llz
runs:
  using: composite
  steps:
    - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6.5.0
`

func run(t *testing.T, root string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	return out.String(), errOut.String(), err
}

// TestCleanTreePasses is the baseline: the composite uses the action, nothing
// else does.
func TestCleanTreePasses(t *testing.T) {
	root := t.TempDir()
	write(t, root, soleSite, composite)
	write(t, root, ".github/workflows/lint.yml", "jobs:\n  a:\n    steps:\n      - uses: ./.github/actions/setup-llz\n")

	out, _, err := run(t, root)
	if err != nil {
		t.Fatalf("clean tree must pass, got: %v", err)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("want an OK line, got %q", out)
	}
}

// TestBypassIsCaught injects the EXACT defect this gate is named for — the second
// setup-go pin release-e2e-lane.yml had grown — and requires it to fire.
//
// This is the arm that matters. A gate proven only against an empty tree proves
// it can fail, not that it catches the class it claims.
func TestBypassIsCaught(t *testing.T) {
	root := t.TempDir()
	write(t, root, soleSite, composite)
	write(t, root, ".github/workflows/release-e2e-lane.yml", `jobs:
  functional:
    steps:
      - uses: actions/checkout@aaaa # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: tools/go.mod
      - run: cd tools && go build -o ../bin/llz ./cmd/llz
`)

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("a hand-rolled setup-go must fail the gate")
	}
	if !strings.Contains(errOut, "release-e2e-lane.yml:5") {
		t.Errorf("want the offending file:line reported, got %q", errOut)
	}
	// The remedy must be in the message: the reader has to know what to write
	// instead, not merely that they were wrong.
	if !strings.Contains(errOut, "setup-llz") {
		t.Errorf("want the composite named as the fix, got %q", errOut)
	}
}

// TestBypassInInstanceTemplateIsCaught pins the second scan root. The rule has to
// hold in what adopters carry, and a guard that silently covered only .github
// would be narrower than its name.
func TestBypassInInstanceTemplateIsCaught(t *testing.T) {
	root := t.TempDir()
	write(t, root, soleSite, composite)
	write(t, root, "instance-template/.github/workflows/llz-x.yml",
		"jobs:\n  a:\n    steps:\n      - uses: actions/setup-go@deadbeef # v1\n")

	if _, errOut, err := run(t, root); err == nil {
		t.Fatalf("a bypass under instance-template must fail; stderr=%q", errOut)
	}
}

// TestCommentMentionIsNotAViolation guards the gate against its own false
// positive: the composite's header explains itself by NAMING the action, and
// several workflows discuss it in comments. Reporting those would make the gate
// a nuisance and get it switched off.
func TestCommentMentionIsNotAViolation(t *testing.T) {
	root := t.TempDir()
	write(t, root, soleSite, composite)
	write(t, root, ".github/workflows/notes.yml", `# This job deliberately avoids actions/setup-go@v6 — see setup-llz.
#   - uses: actions/setup-go@1234
jobs:
  a:
    steps:
      - uses: ./.github/actions/setup-llz
`)

	if _, errOut, err := run(t, root); err != nil {
		t.Fatalf("a commented mention must not fire: %v\n%s", err, errOut)
	}
}

// TestVanishedAuthorityFails is the fail-closed arm that a naive implementation
// gets wrong: delete the composite and every bypass becomes legal, so the gate
// would pass having verified nothing.
func TestVanishedAuthorityFails(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".github/actions/setup-llz/action.yml", "name: Set up llz\nruns:\n  using: composite\n  steps: []\n")
	write(t, root, ".github/workflows/lint.yml", "jobs:\n  a:\n    steps:\n      - uses: actions/setup-go@x\n")

	_, errOut, err := run(t, root)
	if err == nil {
		t.Fatal("composite no longer using setup-go must fail closed")
	}
	if !strings.Contains(err.Error(), "does not use actions/setup-go") {
		t.Errorf("want the vanished-authority reason, got %v (%s)", err, errOut)
	}
}

// TestEmptyCorpusFails covers a wrong --root: no Actions YAML at all must be an
// error, not "OK — 0 scanned".
func TestEmptyCorpusFails(t *testing.T) {
	root := t.TempDir()
	if _, _, err := run(t, root); err == nil {
		t.Fatal("an empty corpus must fail closed")
	}
}

// TestFindSitesLineNumbers pins the 1-based line arithmetic the error message
// depends on; an off-by-one sends the reader to the wrong step.
func TestFindSitesLineNumbers(t *testing.T) {
	body := "a: 1\nb: 2\n      - uses: actions/setup-go@abc\n"
	got := findSites("f.yml", body)
	if len(got) != 1 {
		t.Fatalf("want 1 site, got %d", len(got))
	}
	if got[0].line != 3 {
		t.Errorf("want line 3, got %d", got[0].line)
	}
}
