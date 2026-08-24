package summarysecret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// theOriginalDefect is ci_openbao_init.go as it stood before the fix, reduced to
// the shape that matters: six values masked, and the payload they came from
// appended to the job summary. This is the fixture the guard exists for — if it
// ever stops being a finding, the guard has stopped working.
const theOriginalDefect = `package openbao

func RunInit(region string) error {
	initOut, _ := baoInit()
	res, _ := ParseInit(initOut)
	ghsecret.Mask(res.RootToken)
	for _, k := range res.RecoveryKeysB64 {
		ghsecret.Mask(k)
	}
	return ghaout.Append("GITHUB_STEP_SUMMARY",
		"## OpenBao Initialized — Save These Keys Now",
		"` + "```json" + `",
		strings.TrimSpace(initOut),
		"` + "```" + `")
}
`

func TestScanCatchesTheOriginalDefect(t *testing.T) {
	got, err := scan("a.go", theOriginalDefect)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(got), got)
	}
	if got[0].fn != "RunInit" {
		t.Errorf("fn = %q, want RunInit", got[0].fn)
	}
	if !strings.Contains(got[0].expr, "initOut") {
		t.Errorf("expr = %q, want it to name the payload", got[0].expr)
	}
	if got[0].key() != "a.go:RunInit" {
		t.Errorf("key = %q, want a.go:RunInit", got[0].key())
	}
}

// The extraction that would have defeated a function-scoped rule: Mask stays in
// one function, the append moves to a helper. File scope is what survives it.
func TestScanSeesThroughHelperExtraction(t *testing.T) {
	const src = `package openbao

func RunInit() error {
	res, _ := ParseInit(initOut)
	ghsecret.Mask(res.RootToken)
	return deliver(initOut)
}

func deliver(initOut string) error {
	return ghaout.Append("GITHUB_STEP_SUMMARY", strings.TrimSpace(initOut))
}
`
	got, err := scan("a.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].fn != "deliver" {
		t.Fatalf("findings = %+v, want one in deliver()", got)
	}
}

func TestScanIgnoresFilesThatHandleNoSecrets(t *testing.T) {
	const src = `package x

func Report(region string) error {
	return ghaout.Append("GITHUB_STEP_SUMMARY", fmt.Sprintf("region %s", region))
}
`
	got, err := scan("a.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %+v, want none — nothing here masks, so the strict rule does not apply", got)
	}
}

func TestScanAllowsLiteralsInMaskingFiles(t *testing.T) {
	const src = `package x

func Run(tok string) error {
	ghsecret.Mask(tok)
	return ghaout.Append("GITHUB_STEP_SUMMARY",
		"## Done",
		"",
		"Copy the value from the environment secret. "+
			"It is not printed here.")
}
`
	got, err := scan("a.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %+v, want none — every argument is a string literal", got)
	}
}

// Other GHA output files are a different channel: $GITHUB_ENV and $GITHUB_OUTPUT
// are consumed by later steps, not rendered for readers, and the init lane
// legitimately writes the token to both.
func TestScanOnlyGuardsTheStepSummary(t *testing.T) {
	const src = `package x

func Run(tok string) error {
	ghsecret.Mask(tok)
	return ghaout.Append("GITHUB_ENV", "OPENBAO_ROOT_TOKEN="+tok)
}
`
	got, err := scan("a.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("findings = %+v, want none — $GITHUB_ENV is not the rendered channel", got)
	}
}

// A method's key carries its receiver, so two same-named methods cannot share one
// registry entry.
func TestScanKeysMethodsByReceiver(t *testing.T) {
	const src = `package x

func (h *harborAPI) createRobot(name string) error {
	ghsecret.Mask(name)
	return ghaout.Append("GITHUB_STEP_SUMMARY", fmt.Sprintf("robot %s exists", name))
}
`
	got, err := scan("a.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].key() != "a.go:harborAPI.createRobot" {
		t.Fatalf("findings = %+v, want key a.go:harborAPI.createRobot", got)
	}
}

// FAIL CLOSED. A file this cannot parse is a file whose summary writes it cannot
// see, and reporting green over it launders an absence of evidence.
func TestScanFailsOnUnparseableGo(t *testing.T) {
	if _, err := scan("a.go", "package x\nfunc broken( {"); err == nil {
		t.Error("scan = nil error on unparseable Go, want a failure")
	}
}

func TestIsStringLiteral(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`"plain"`, true},
		{`"a" + "b"`, true},
		{`("nested")`, true},
		{`"a" + b`, false},
		{`fmt.Sprintf("%s", x)`, false},
		{`x`, false},
		{"`raw`", true},
	}
	for _, tc := range cases {
		src := "package x\nfunc f(tok string) error {\n\tghsecret.Mask(tok)\n\treturn ghaout.Append(\"GITHUB_STEP_SUMMARY\", " + tc.src + ")\n}\n"
		got, err := scan("a.go", src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if literal := len(got) == 0; literal != tc.want {
			t.Errorf("isStringLiteral(%s) = %v, want %v", tc.src, literal, tc.want)
		}
	}
}

// relForKey must produce the same key whether the guard was pointed at the
// template checkout or at an instance, where the tree sits one level down.
func TestRelForKey(t *testing.T) {
	for _, in := range []string{
		"tools/internal/x/y.go",
		"./tools/internal/x/y.go",
		"instance-template/tools/internal/x/y.go",
	} {
		if got := relForKey(in); got != "tools/internal/x/y.go" {
			t.Errorf("relForKey(%q) = %q", in, got)
		}
	}
}

// The registry is the guard's reviewable surface; an entry with no reason is a
// rubber stamp, which is the failure mode plaintextAllowed's bar exists to stop.
func TestEveryRegistryEntryCarriesAReason(t *testing.T) {
	for key, rule := range summaryComputedAllowed {
		if len(strings.Fields(rule.reason)) < 8 {
			t.Errorf("summaryComputedAllowed[%q].reason is too thin to review: %q", key, rule.reason)
		}
		if !strings.Contains(key, ".go:") {
			t.Errorf("summaryComputedAllowed key %q is not <file>.go:<function>", key)
		}
	}
}

// ── the real tree, and the fail-closed arms ─────────────────────────────────

// TestGuardRealTree runs the guard over the actual repo. It is the test that
// fails when someone adds an unregistered computed summary write in a file that
// masks — the whole point of the guard — and equally when a registry entry
// outlives its call site.
func TestGuardRealTree(t *testing.T) {
	if err := Run(repoRootForGuardTest(t)); err != nil {
		t.Errorf("summary-secret-guard failed on the real tree: %v", err)
	}
}

// A guard pointed at nothing must not report the same green as one that read
// every file. Mirrors the RequireCorpus contract the sibling guards share.
func TestGuardFailsOnEmptyCorpus(t *testing.T) {
	err := Run(t.TempDir())
	if err == nil {
		t.Fatal("expected a failure on an empty corpus")
	}
	if !strings.Contains(err.Error(), "empty corpus") {
		t.Errorf("error should name the empty corpus, got: %v", err)
	}
}

// An unparseable file inside the corpus must fail the run, not be walked past:
// a file the guard cannot read is a file whose summary writes it cannot see.
func TestCollectFailsOnUnparseableFileInTree(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tools", "internal", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package x\nfunc ( {"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(root); err == nil || !strings.Contains(err.Error(), "did not parse") {
		t.Errorf("err = %v, want the parse failure — an unreadable file must not pass green", err)
	}
}

// The guard's own source names every symbol it searches for, in its registry keys
// and its finding message, so without the self-exemption it reports itself.
// EXEMPT BY DIRECTORY, NOT BY FILENAME: plaintext-guard's extraction scar was that
// a basename list stopped matching the moment the file moved within the tree.
func TestGuardExemptsItselfByDirectory(t *testing.T) {
	root := repoRootForGuardTest(t)
	repo := capability.RepoForGate(Extension(), root)
	own := filepath.Join("tools", "internal", "extensions", "guards", "summarysecret", "guard.go")
	if _, err := repo.Stat(own); err != nil {
		t.Fatalf("%s not found — guardOwnDir no longer matches this package's real source: %v", own, err)
	}
	if !strings.Contains(filepath.ToSlash(own), guardOwnDir) {
		t.Errorf("guardOwnDir %q does not match this package's own file %q", guardOwnDir, own)
	}
}

// A file that masks but writes only literals contributes no findings, and a tree
// of them still counts toward the corpus — otherwise RequireCorpus would fire on
// a perfectly clean repo.
func TestCollectCountsCleanFilesTowardTheCorpus(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tools", "internal", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package x\n\nfunc Run(tok string) error {\n\tghsecret.Mask(tok)\n\treturn ghaout.Append(\"GITHUB_STEP_SUMMARY\", \"done\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "clean.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := capability.RepoForGate(Extension(), root)
	got, examined, err := collect(repo, []string{"tools"})
	if err != nil {
		t.Fatal(err)
	}
	if examined != 1 {
		t.Errorf("examined = %d, want 1", examined)
	}
	if len(got) != 0 {
		t.Errorf("findings = %+v, want none", got)
	}
}

// _test.go files are excluded: a test that constructs the defect as a FIXTURE
// (this file does, above) is not the defect.
func TestCollectSkipsTestFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tools", "internal", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(theOriginalDefect), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := capability.RepoForGate(Extension(), root)
	_, examined, err := collect(repo, []string{"tools"})
	if err != nil {
		t.Fatal(err)
	}
	if examined != 0 {
		t.Errorf("examined = %d, want 0 — _test.go files are not the shipped surface", examined)
	}
}

func repoRootForGuardTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "platform-apl")); err != nil {
		t.Skipf("repo root not found at %s: %v", root, err)
	}
	return root
}
