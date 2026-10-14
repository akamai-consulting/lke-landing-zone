package tfenc

// tfenc_test.go — two gates: what Go emits is what the composite action emits, and
// what either emits satisfies the shipped roots. Both RUN the other side rather
// than restating its format, because a fixture agrees with whatever it was copied
// from and keeps agreeing after that side changes.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfroots"
)

// actionPath is the composite action this package must agree with, byte for byte.
const actionPath = "instance-template/.github/actions/tf-encryption-env/action.yml"

// repoRoot walks up to the template repo root — the directory holding the
// composite action under test.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, actionPath)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("%s not found above %s — this gate compares Go against the SHIPPED action, "+
				"so a moved or deleted action must fail here rather than quietly stop being checked", actionPath, dir)
		}
		dir = parent
	}
}

// actionScript extracts the composite action's real shell body and the names of
// the environment variables it reads.
func actionScript(t *testing.T) (script string, envNames []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), actionPath))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runs struct {
			Steps []struct {
				Run string            `json:"run"`
				Env map[string]string `json:"env"`
			} `json:"steps"`
		} `json:"runs"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parsing %s: %v", actionPath, err)
	}
	if len(doc.Runs.Steps) != 1 || strings.TrimSpace(doc.Runs.Steps[0].Run) == "" {
		t.Fatalf("%s no longer has exactly one `run:` step — this gate executes that body, "+
			"so it must be re-taught rather than left pointing at the wrong step", actionPath)
	}
	for k := range doc.Runs.Steps[0].Env {
		envNames = append(envNames, k)
	}
	return doc.Runs.Steps[0].Run, envNames
}

// runAction executes the action's shell body and returns the TF_ENCRYPTION value
// it wrote to $GITHUB_ENV, or the action's own error output.
//
// The action emits a heredoc block (TF_ENCRYPTION<<TF_ENCRYPTION_EOF … EOF),
// which is what GitHub Actions supports for a multi-line value. Parsing it back
// out is the only translation this helper performs.
func runAction(t *testing.T, env map[string]string) (string, error) {
	t.Helper()
	script, _ := actionScript(t)
	ghEnv := filepath.Join(t.TempDir(), "github_env")
	if err := os.WriteFile(ghEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "GITHUB_ENV="+ghEnv)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	body, rerr := os.ReadFile(ghEnv)
	if rerr != nil {
		t.Fatal(rerr)
	}
	const open, close = "TF_ENCRYPTION<<TF_ENCRYPTION_EOF\n", "TF_ENCRYPTION_EOF\n"
	s := string(body)
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("the action wrote no TF_ENCRYPTION heredoc to $GITHUB_ENV:\n%s", s)
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("the action's TF_ENCRYPTION heredoc is unterminated:\n%s", s)
	}
	return rest[:j], nil
}

// TestBuildMatchesTheShippedCompositeAction is the coupling gate. Two emitters,
// one document: if they disagree about the key-provider name, the method name or
// which methods are declared, state written by one is UNREADABLE by the other —
// surfacing as a failed apply mid-provisioning, or a rotation that reports success
// having verified nothing. The shell body is read out of the shipped action.yml
// and executed, not paraphrased.
func TestBuildMatchesTheShippedCompositeAction(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to execute the composite action's real shell body")
	}
	// Names in the action's `env:` block are the contract this test drives it
	// through; a rename there must fail here rather than silently start testing
	// the empty string.
	_, names := actionScript(t)
	for _, want := range []string{"PASSPHRASE", "KEY_NAME", "PASSPHRASE_OLD", "KEY_NAME_OLD"} {
		if !contains(names, want) {
			t.Fatalf("the action no longer reads %s (it reads %v) — this gate drives it through "+
				"those names, so the rename must be reflected here", want, names)
		}
	}

	for _, tc := range []struct {
		name string
		cfg  Config
		env  map[string]string
	}{
		{
			name: "steady state",
			cfg:  Config{Passphrase: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", KeyName: "llz"},
			env:  map[string]string{"PASSPHRASE": "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", "KEY_NAME": "llz"},
		},
		{
			name: "steady state, non-default key name",
			cfg:  Config{Passphrase: "c2Vjb25kLXBhc3NwaHJhc2UtdmFsdWU=", KeyName: "llz_g2"},
			env:  map[string]string{"PASSPHRASE": "c2Vjb25kLXBhc3NwaHJhc2UtdmFsdWU=", "KEY_NAME": "llz_g2"},
		},
		{
			// The rotation window is the case worth pinning hardest: it is the one
			// that emits a fallback, it runs rarely, and getting the fallback's key
			// name wrong strands state on a passphrase about to be deleted.
			name: "rotation window",
			cfg: Config{
				Passphrase: "bmV3LWtleS1wYXNzcGhyYXNlLXZhbHVl", KeyName: "llz_g2",
				PassphraseOld: "b2xkLWtleS1wYXNzcGhyYXNlLXZhbHVl", KeyNameOld: "llz",
			},
			env: map[string]string{
				"PASSPHRASE": "bmV3LWtleS1wYXNzcGhyYXNlLXZhbHVl", "KEY_NAME": "llz_g2",
				"PASSPHRASE_OLD": "b2xkLWtleS1wYXNzcGhyYXNlLXZhbHVl", "KEY_NAME_OLD": "llz",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromShell, err := runAction(t, tc.env)
			if err != nil {
				t.Fatalf("the composite action failed on input this package accepts:\n%s", fromShell)
			}
			fromGo, err := Build(tc.cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if fromGo != fromShell {
				t.Errorf("Go and the composite action emit DIFFERENT encryption configuration.\n"+
					"State written under one is not readable by the other.\n--- go ---\n%s\n--- action ---\n%s",
					fromGo, fromShell)
			}
		})
	}
}

// TestBuildAndActionRejectTheSameInputs pins the guards, not just the happy path.
//
// The character class and the length floor are a SECURITY boundary — the
// passphrase is interpolated into an HCL string, where a quote can close it and
// append `method "unencrypted"`, writing plaintext state that still looks
// encrypted from outside. One side accepting what the other rejects is how that
// boundary moves: the value passes locally, reaches CI, and fails there — or
// worse, passes both and means different things.
func TestBuildAndActionRejectTheSameInputs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to execute the composite action's real shell body")
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		env  map[string]string
	}{
		{"empty passphrase", Config{KeyName: "llz"}, map[string]string{"PASSPHRASE": "", "KEY_NAME": "llz"}},
		{"quote in passphrase", Config{Passphrase: `a" } method "unencrypted" "x" {}`, KeyName: "llz"},
			map[string]string{"PASSPHRASE": `a" } method "unencrypted" "x" {}`, "KEY_NAME": "llz"}},
		{"passphrase below the pbkdf2 floor", Config{Passphrase: "short", KeyName: "llz"},
			map[string]string{"PASSPHRASE": "short", "KEY_NAME": "llz"}},
		{"hyphen in key name", Config{Passphrase: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", KeyName: "llz-g2"},
			map[string]string{"PASSPHRASE": "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", "KEY_NAME": "llz-g2"}},
		{"rotation without the old key name",
			Config{Passphrase: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", KeyName: "llz_g2", PassphraseOld: "b2xkLXBhc3NwaHJhc2UtdmFsdWUtaGVyZQ=="},
			map[string]string{"PASSPHRASE": "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", "KEY_NAME": "llz_g2", "PASSPHRASE_OLD": "b2xkLXBhc3NwaHJhc2UtdmFsdWUtaGVyZQ=="}},
		{"rotation reusing the same key name",
			Config{Passphrase: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", KeyName: "llz", PassphraseOld: "b2xkLXBhc3NwaHJhc2UtdmFsdWUtaGVyZQ==", KeyNameOld: "llz"},
			map[string]string{"PASSPHRASE": "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", "KEY_NAME": "llz", "PASSPHRASE_OLD": "b2xkLXBhc3NwaHJhc2UtdmFsdWUtaGVyZQ==", "KEY_NAME_OLD": "llz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(tc.cfg); err == nil {
				t.Error("Build accepted it; the composite action does not")
			}
			if _, err := runAction(t, tc.env); err == nil {
				t.Error("the composite action accepted it; Build does not")
			}
		})
	}
}

// TestBuildSatisfiesTheShippedRoots is the behavior gate: does the document this
// package emits actually make OpenTofu run?
//
// Everything else here compares two emitters to each other, which would stay
// green if BOTH were wrong. This one asserts the property adopters care about, on
// the real artifact — the `encryption.tf` embedded in tfroots and rendered into
// every instance — by running OpenTofu against it.
//
// It is hermetic: encryption.tf alone declares no providers, no backend and no
// resources, so `tofu init` reaches the encryption block and stops. No network,
// no credentials, no state.
//
// Both arms matter. Without the document, init must FAIL — that failure is the
// tripwire ADR 0007 relies on to prevent a plaintext write, and a change that
// quietly made encryption optional would otherwise pass every other check here.
func TestBuildSatisfiesTheShippedRoots(t *testing.T) {
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("OpenTofu is required to prove the emitted document satisfies the roots")
	}
	files := tfroots.Files(".", "example-org", "v0.0.0")
	var encryption string
	for path, body := range files {
		if filepath.Base(path) == "encryption.tf" && strings.Contains(path, "cluster") {
			encryption = body
		}
	}
	if encryption == "" {
		t.Fatal("the cluster root ships no encryption.tf — state-at-rest encryption is the " +
			"thing this package configures, so its absence is the regression, not a reason to skip")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "encryption.tf"), []byte(encryption), 0o600); err != nil {
		t.Fatal(err)
	}
	initIn := func(env []string) (string, error) {
		cmd := exec.Command("tofu", "init", "-input=false")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Arm 1 — no document: OpenTofu must refuse. This is the tripwire.
	if out, err := initIn([]string{EnvVar + "="}); err == nil {
		t.Errorf("`tofu init` SUCCEEDED against the shipped encryption.tf with no %s.\n"+
			"That tripwire is what stops a hand-run apply from writing plaintext state (ADR 0007).\n%s", EnvVar, out)
	}

	// Arm 2 — this package's document: OpenTofu must accept it.
	doc, err := Build(Config{Passphrase: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", KeyName: DefaultKeyName})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := initIn([]string{EnvVar + "=" + doc}); err != nil {
		t.Errorf("`tofu init` REJECTED the document this package emits — every hand-run "+
			"OpenTofu command in an instance would fail with it:\n%s", out)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestExportsSurviveARealShellAndSatisfyTheRoots gates `eval "$(llz tofu
// --export)"`. Inspecting the exports as a STRING cannot see the failure that
// matters: TF_ENCRYPTION is multi-line, and a quoting slip produces no shell error
// — just a truncated document, after which OpenTofu reports the same "Invalid
// expression" as having none. So eval it in a real shell and hand the result to a
// real `tofu init` against the real shipped root.
func TestExportsSurviveARealShellAndSatisfyTheRoots(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to prove the exports survive a shell")
	}
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("OpenTofu is required to prove what the shell exported is usable")
	}
	doc, err := Build(Config{Passphrase: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", KeyName: DefaultKeyName})
	if err != nil {
		t.Fatal(err)
	}
	local := Local{Vars: []Var{{Name: EnvVar, Value: doc}}}

	files := tfroots.Files(".", "example-org", "v0.0.0")
	dir := t.TempDir()
	for path, body := range files {
		if filepath.Base(path) == "encryption.tf" && strings.Contains(path, "cluster") {
			if err := os.WriteFile(filepath.Join(dir, "encryption.tf"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Through the REAL handoff — the private file and the source-and-delete
	// snippet — not through Exports() directly, because the file is what an
	// operator's shell actually consumes and the quoting has one more layer.
	snippet, err := local.WriteExports()
	if err != nil {
		t.Fatal(err)
	}
	// `eval` in the same shell that runs tofu, exactly as an operator would.
	cmd := exec.Command("bash", "-c", `eval "$SNIPPET"; tofu init -input=false`)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "SNIPPET="+snippet, EnvVar+"=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("`eval \"$(llz tofu --export)\"` did not produce a usable environment — the "+
			"documented one-liner is broken:\n%s", out)
	}
}

// WriteExports puts a live passphrase on disk for the duration of one source.
// Two things have to hold for that to be an improvement over printing it, and
// neither is visible from the snippet: the path must be private BEFORE the file
// exists, and the bytes must be the exports.
func TestWriteExportsIsPrivateBeforeTheFileExists(t *testing.T) {
	l := Local{Vars: []Var{{Name: EnvVar, Value: "secret-doc"}}}
	snippet, err := l.WriteExports()
	if err != nil {
		t.Fatal(err)
	}
	path := snippetPath(t, snippet)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })

	// THE DIRECTORY IS WHAT CARRIES THE GUARANTEE. A 0600 file created inside a
	// world-readable /tmp is still visible to every local user between create and
	// chmod; os.MkdirTemp makes the private directory first, so there is no such
	// instant.
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("export directory mode = %v, want 0700 — another local user can list it", di.Mode().Perm())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("export file mode = %v, want 0600", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != l.Exports() {
		t.Errorf("the file does not carry the exports:\n%s", b)
	}
}

// THE HAZARD THE SWEEPER EXISTS FOR: `--export` run without an `eval` writes a
// passphrase nobody sources, and nothing else would ever remove it. That is
// strictly worse than the stdout it replaced — a secret on disk indefinitely,
// with nothing on screen saying so.
//
// The age threshold is equally load-bearing in the other direction: sweeping
// indiscriminately would delete a CONCURRENT invocation's file mid-handoff, so a
// fresh file must survive.
func TestSweepRemovesAbandonedExportsButNotFreshOnes(t *testing.T) {
	l := Local{Vars: []Var{{Name: EnvVar, Value: "secret-doc"}}}

	stale, err := l.WriteExports()
	if err != nil {
		t.Fatal(err)
	}
	stalePath := snippetPath(t, stale)
	old := time.Now().Add(-2 * exportMaxAge)
	if err := os.Chtimes(filepath.Dir(stalePath), old, old); err != nil {
		t.Fatal(err)
	}

	fresh, err := l.WriteExports()
	if err != nil {
		t.Fatal(err)
	}
	freshPath := snippetPath(t, fresh)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(freshPath)) })

	SweepStaleExports()

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("an abandoned export file survived the sweep (err=%v) — that is a passphrase "+
			"left on disk with nothing to clean it up", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("the sweep deleted a FRESH export file, which would break a concurrent "+
			"`eval` mid-handoff: %v", err)
	}
}

// snippetPath pulls the sourced file out of `. 'PATH'; rm -f 'PATH'; rmdir …`.
//
// WRITTEN OUT PROPERLY BECAUSE THE OBVIOUS VERSION IS A FALSE PASS.
// `strings.Fields(snippet)[1]` yields `'PATH';` — trailing semicolon included —
// and trimming quotes off that leaves a path that never existed, so every
// "the file is gone" assertion passed by checking the wrong name.
func snippetPath(t *testing.T, snippet string) string {
	t.Helper()
	i := strings.Index(snippet, "'")
	if i < 0 {
		t.Fatalf("no quoted path in snippet: %q", snippet)
	}
	j := strings.Index(snippet[i+1:], "'")
	if j < 0 {
		t.Fatalf("unterminated quoted path in snippet: %q", snippet)
	}
	return snippet[i+1 : i+1+j]
}
