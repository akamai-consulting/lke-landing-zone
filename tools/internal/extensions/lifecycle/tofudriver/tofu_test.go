package tofudriver

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/exitcode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfenc"
)

const testPassphrase = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY"

// instanceAt builds a throwaway instance checkout with a `.llz` cache and the
// four Terraform roots, and chdirs into the named one.
func instanceAt(t *testing.T, root string, secrets map[string]string) string {
	t.Helper()
	for _, k := range []string{
		tfenc.EnvVar, tfenc.PassphraseEnv, tfenc.KeyNameEnv, tfenc.PassphraseOldEnv, tfenc.KeyNameOldEnv,
		"TF_STATE_ACCESS_KEY", "TF_STATE_SECRET_KEY", "TF_STATE_ENDPOINT", "TF_STATE_BUCKET",
		"LINODE_API_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_ENDPOINT_URL_S3", "LINODE_TOKEN",
	} {
		t.Setenv(k, "")
	}
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, ".copier-answers.yml"), []byte("_src_path: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A spec, because a spec-driven instance is the normal one and `llz render` —
	// which the unrendered-root refusal names — only works where there is one.
	// The content is never parsed here; its presence is what the refusal branches on.
	if err := os.WriteFile(filepath.Join(base, clusterspec.LandingZoneFile),
		[]byte("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, ".llz"), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for k, v := range secrets {
		b.WriteString(k + "=" + v + "\n")
	}
	if err := os.WriteFile(filepath.Join(base, tfenc.SecretsFile), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "terraform-iac-bootstrap", root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// RENDERED, because that is what a checkout someone is running OpenTofu in
	// looks like. This fixture used to leave the root empty, which every test here
	// then silently relied on — and an empty root is the one state where the real
	// command now refuses, so the fixture was modelling the broken case as normal.
	// The content is irrelevant (nothing here parses it); its presence is the whole
	// signal, exactly as it is for `llz render`'s output.
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("# rendered by llz render\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return base
}

// unrender removes the `*.tf` from the root the test is standing in, putting it
// back in the state of a fresh clone.
func unrender(t *testing.T) {
	t.Helper()
	matches, err := filepath.Glob("*.tf")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatal(err)
		}
	}
}

func fullSecrets() map[string]string {
	return map[string]string{
		tfenc.PassphraseEnv:   testPassphrase,
		"TF_STATE_ACCESS_KEY": "ak",
		"TF_STATE_SECRET_KEY": "sk",
		"TF_STATE_ENDPOINT":   "https://us-east-1.linodeobjects.com",
		"TF_STATE_BUCKET":     "state-bucket",
		"LINODE_API_TOKEN":    "pat",
	}
}

// THE STATE KEY IS THE DANGEROUS PART OF THIS COMMAND. It selects which cluster's
// state the directory operates on, and OpenTofu does not validate it: a key
// nothing has written initializes cleanly against an EMPTY state, after which
// `plan` proposes building a second cluster. So the key is only ever DERIVED from
// something the operator typed.
func TestBackendConfigIsOnlyDerivedFromAnExplicitRegion(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	local, err := tfenc.Hydrate(".")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no region: init is left exactly as typed", func(t *testing.T) {
		got, err := withBackendConfig(TofuOpts{}, local, []string{"init", "-upgrade"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("an init without --region must not acquire a backend key it was never given: %v", got)
		}
	})

	t.Run("region: the key names this root and that deployment", func(t *testing.T) {
		got, err := withBackendConfig(TofuOpts{Region: "primary"}, local, []string{"init"})
		if err != nil {
			t.Fatal(err)
		}
		want := "-backend-config=key=cluster/primary/terraform.tfstate"
		if !contains(got, want) {
			t.Errorf("want %q in %v", want, got)
		}
		if !contains(got, "-backend-config=bucket=state-bucket") {
			t.Errorf("the bucket must come from the cache, got %v", got)
		}
	})

	t.Run("a caller's own -backend-config is never second-guessed", func(t *testing.T) {
		in := []string{"init", "-backend-config=key=cluster/other/terraform.tfstate"}
		got, err := withBackendConfig(TofuOpts{Region: "primary"}, local, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(in) {
			t.Errorf("llz appended a second, CONFLICTING state key to an explicit one: %v", got)
		}
	})

	t.Run("only init", func(t *testing.T) {
		got, err := withBackendConfig(TofuOpts{Region: "primary"}, local, []string{"plan"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("backend config belongs to init alone, got %v", got)
		}
	})

	t.Run("--state-key overrides, for the VPC root keyed by network", func(t *testing.T) {
		got, err := withBackendConfig(TofuOpts{StateKey: "vpc/shared-a/terraform.tfstate"}, local, []string{"init"})
		if err != nil {
			t.Fatal(err)
		}
		if !contains(got, "-backend-config=key=vpc/shared-a/terraform.tfstate") {
			t.Errorf("want the explicit key, got %v", got)
		}
	})
}

// Standing in the wrong directory is the reachable mistake, and it is the one
// case where guessing would be silently destructive rather than merely wrong.
func TestBackendConfigRefusesToGuessOutsideAKnownRoot(t *testing.T) {
	instanceAt(t, "not-a-root", fullSecrets())
	local, _ := tfenc.Hydrate(".")
	_, err := withBackendConfig(TofuOpts{Region: "primary"}, local, []string{"init"})
	if err == nil {
		t.Fatal("llz derived a state key from a directory that is not a Terraform root")
	}
	if !strings.Contains(err.Error(), "--state-key") {
		t.Errorf("the refusal must offer the way through, got: %v", err)
	}
}

// The error this command exists to replace. It must name the SECRET and the
// command that gathers it — an operator told "TF_ENCRYPTION is unset" goes and
// writes the twelve-line document by hand, which is the status quo.
func TestMissingPassphraseNamesTheSecretAndTheRemedy(t *testing.T) {
	instanceAt(t, "cluster", map[string]string{"TF_STATE_ACCESS_KEY": "ak"})
	var out, errBuf bytes.Buffer
	err := RunTofu(&out, &errBuf, TofuOpts{}, []string{"plan"})
	if err == nil {
		t.Fatal("a checkout with no passphrase must not reach OpenTofu")
	}
	for _, want := range []string{tfenc.PassphraseEnv, "llz tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message must mention %q, got:\n%v", want, err)
		}
	}
}

// A live instance's passphrase has been disclosed by writing these values to
// stdout, which is captured by scrollback, script(1), CI logs and `set -x`. So the
// assertion is not "the exports look right" — it is that the secret is NOT THERE,
// checked against the raw bytes of stdout.
func TestExportPutsNoSecretOnStdout(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	var out, errBuf bytes.Buffer
	if err := RunTofu(&out, &errBuf, TofuOpts{Export: true}, nil); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassphrase, "sk", "pat"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("a secret reached stdout, where every recorder can see it:\n%s", out.String())
		}
	}
	if !strings.HasPrefix(out.String(), ". ") {
		t.Errorf("stdout should carry only the source-and-delete snippet, got:\n%s", out.String())
	}
}

// The snippet is fed to `eval`. Proving it in Go would only prove Go's idea of
// it, so this runs the real thing: eval it in a real shell and ask that shell
// what it ended up with, then confirm the file it sourced is gone.
func TestExportSnippetSourcesAndSelfDeletes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to prove the snippet is evaluable")
	}
	instanceAt(t, "cluster", fullSecrets())
	var out, errBuf bytes.Buffer
	if err := RunTofu(&out, &errBuf, TofuOpts{Export: true}, nil); err != nil {
		t.Fatal(err)
	}
	snippet := out.String()

	// The shell reports the length of what it got, never the value.
	script := "eval \"$SNIPPET\"\n" +
		"printf 'enc=%s aws=%s linode=%s\\n' \"${#TF_ENCRYPTION}\" \"$AWS_ACCESS_KEY_ID\" \"$LINODE_TOKEN\"\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "SNIPPET="+snippet)
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("evaluating the snippet failed: %v\n%s", err, got)
	}
	if strings.Contains(string(got), "enc=0") {
		t.Errorf("the shell ended up with an empty TF_ENCRYPTION — the handoff did not deliver:\n%s", got)
	}
	if !strings.Contains(string(got), "aws=ak") || !strings.Contains(string(got), "linode=pat") {
		t.Errorf("the derived variables did not survive the handoff:\n%s", got)
	}

	// The file must be gone: the whole point is that it exists for the duration
	// of one source and no longer.
	path := snippetPath(t, snippet)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s survived the snippet that was supposed to delete it (err=%v)", path, err)
	}
}

// On a terminal there is almost certainly no `eval` around this, so the handoff
// would leave an unconsumed passphrase in a temp file — worse than the printing
// it replaced, because nothing on screen says it happened.
func TestExportRefusesATerminal(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	var out, errBuf bytes.Buffer
	err := RunTofu(&out, &errBuf, TofuOpts{Export: true, StdoutIsTerminal: true}, nil)
	if err == nil {
		t.Fatal("--export ran on a terminal")
	}
	if !strings.Contains(err.Error(), "eval") || !strings.Contains(err.Error(), "--shell-init") {
		t.Errorf("the refusal must show both supported forms, got: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("the refusal still emitted %d bytes to stdout", out.Len())
	}
}

// Outside an instance there is nothing to resolve, and saying so beats running
// OpenTofu with an environment the operator will assume was applied.
func TestRefusesOutsideAnInstanceCheckout(t *testing.T) {
	t.Setenv(tfenc.PassphraseEnv, "")
	t.Chdir(t.TempDir())
	var out, errBuf bytes.Buffer
	err := RunTofu(&out, &errBuf, TofuOpts{}, []string{"version"})
	if err == nil || !strings.Contains(err.Error(), "instance") {
		t.Errorf("want a refusal naming the missing instance checkout, got %v", err)
	}
}

// An incomplete cache is a normal state — `tofu fmt` and `tofu validate` need
// none of these. Reporting must not become refusing.
func TestPartialCacheWarnsRatherThanRefusing(t *testing.T) {
	instanceAt(t, "cluster", map[string]string{tfenc.PassphraseEnv: testPassphrase})
	local, err := tfenc.Hydrate(".")
	if err != nil {
		t.Fatal(err)
	}
	var errBuf bytes.Buffer
	reportResolution(&errBuf, local)
	got := errBuf.String()
	if !strings.Contains(got, "TF_STATE_ACCESS_KEY") {
		t.Errorf("the warning must name the key to set, got: %s", got)
	}
	if !strings.Contains(got, "llz tokens") {
		t.Errorf("the warning must name what gathers it, got: %s", got)
	}
	// "TF_STATE_BUCKET (→ TF_STATE_BUCKET)" reads like a bug in the tool. The
	// arrow is only meaningful when the two names differ.
	if strings.Contains(got, "TF_STATE_BUCKET (→ TF_STATE_BUCKET)") {
		t.Errorf("a variable whose source and target names coincide should print once, got: %s", got)
	}
	// One resolution, one report. This used to be two overlapping lines from two
	// independent readings of the same cache.
	if strings.Count(got, tfenc.SecretsFile) > 2 {
		t.Errorf("the resolution is reported more than once:\n%s", got)
	}
}

// THE PASSTHROUGH MUST NOT BE STRICTER THAN THE THING IT WRAPS.
//
// The encryption gate was unconditional, so `llz tofu -- fmt` was REFUSED in a
// checkout that had never run `llz tokens` — while telling the operator that
// "OpenTofu refuses to run without it", which for `fmt` is untrue. Measured
// against OpenTofu 1.12.6 with the shipped encryption.tf: version/fmt/validate
// run clean without TF_ENCRYPTION; init/plan/providers/graph/show/output/console
// all fail on it.
func TestEncryptionIsRequiredOnlyWhereOpenTofuActuallyNeedsIt(t *testing.T) {
	for _, verb := range []string{"version", "fmt", "validate", "help", "--help"} {
		if needsEncryption([]string{verb}) {
			t.Errorf("%q runs fine with no encryption configured; refusing it makes llz "+
				"stricter than a bare tofu", verb)
		}
	}
	for _, verb := range []string{"init", "plan", "apply", "providers", "graph", "show", "output", "console", "state", "destroy"} {
		if !needsEncryption([]string{verb}) {
			t.Errorf("%q consults the encryption block, so llz must explain the missing "+
				"passphrase rather than let OpenTofu emit \"Invalid expression\"", verb)
		}
	}
	// FAILS CLOSED. A subcommand OpenTofu has not shipped yet must be treated as
	// needing the key: the cost of being wrong that way is a clear message the
	// operator did not strictly need, and the cost of being wrong the other way is
	// the cryptic error this command exists to replace.
	if !needsEncryption([]string{"some-future-verb"}) {
		t.Error("an unknown subcommand must be assumed to need the encryption config")
	}
}

// The end-to-end of the above: a checkout with no passphrase must still be able
// to run the encryption-free verbs.
func TestFmtRunsInACheckoutWithNoPassphrase(t *testing.T) {
	instanceAt(t, "cluster", map[string]string{})
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "tofu"), []byte("#!/bin/sh\necho STUB \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF", filepath.Join(stub, "tofu"))

	var out, errBuf bytes.Buffer
	if err := RunTofu(&out, &errBuf, TofuOpts{}, []string{"fmt"}); err != nil {
		t.Fatalf("`llz tofu -- fmt` was refused in a checkout with no passphrase: %v", err)
	}
	if !strings.Contains(out.String(), "STUB fmt") {
		t.Errorf("fmt did not reach OpenTofu: %q", out.String())
	}

	// …and a state-touching verb in the same checkout must still explain itself.
	err := RunTofu(&out, &errBuf, TofuOpts{}, []string{"plan"})
	if err == nil {
		t.Fatal("`llz tofu -- plan` must not reach OpenTofu with no passphrase")
	}
	if !strings.Contains(err.Error(), "tofu plan") {
		t.Errorf("the refusal should name the verb that needs it, got: %v", err)
	}
}

// A PASSTHROUGH'S EXIT STATUS IS PART OF ITS CONTRACT: `plan -detailed-exitcode`
// returns 2 for "changes pending", which `llz ci tf-plan` gates a workflow on.
// Collapsing it into 1 makes it indistinguishable from a crash.
func TestChildExitStatusSurvives(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "tofu"),
		[]byte("#!/bin/sh\n[ \"$1\" = plan ] && exit 2\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF", filepath.Join(stub, "tofu"))

	var out, errBuf bytes.Buffer
	err := RunTofu(&out, &errBuf, TofuOpts{}, []string{"plan", "-detailed-exitcode"})
	var p *exitcode.Passthrough
	if !errors.As(err, &p) {
		t.Fatalf("want a Passthrough carrying OpenTofu's status, got %T: %v", err, err)
	}
	if p.Code != 2 {
		t.Errorf("exit status = %d, want 2 — `plan -detailed-exitcode` means CHANGES, not failure", p.Code)
	}
	// And the reporting half: the child already printed its own diagnostics, so
	// adding "llz: exit status 2" after them is noise on top of the real message.
	var reported bytes.Buffer
	if got := exitcode.Report(&reported, err); got != 2 {
		t.Errorf("Report returned %d, want 2", got)
	}
	if reported.Len() != 0 {
		t.Errorf("Report spoke over the child's own diagnostics: %q", reported.String())
	}
}

// The rc snippet is pasted into shells and then forgotten about. Two properties
// have to hold or it breaks a machine rather than helping it: it must not recurse
// (the function calls llz, which execs the BINARY, not the shell function), and
// it must degrade to the real `tofu` when llz is not installed.
func TestShellInitIsSafeToSourceInARealShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to prove the rc snippet is sourceable")
	}
	snippet := ShellInit()
	if !strings.Contains(snippet, "command tofu") {
		t.Error("the snippet must fall through to the real binary when llz is absent — " +
			"otherwise installing it breaks tofu on every machine that later loses llz")
	}
	// Source it with llz absent from PATH and prove the fallback runs the real
	// binary rather than looping. A stub `tofu` on PATH reports that it was called.
	dir := t.TempDir()
	stub := filepath.Join(dir, "tofu")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho STUB-TOFU \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", snippet+"\ntofu version\n")
	cmd.Env = append(os.Environ(), "PATH="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sourcing the snippet and calling tofu failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "STUB-TOFU version") {
		t.Errorf("the fallback did not reach the real binary (infinite recursion is the "+
			"failure this catches):\n%s", out)
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

// snippetPath pulls the sourced file out of `. 'PATH'; rm -f 'PATH'; rmdir …`.
//
// Written out properly because the obvious version is a false pass:
// `strings.Fields(snippet)[1]` yields `'PATH';`, and trimming quotes off that
// leaves a path that never existed — so "the file is gone" checks the wrong name.
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

// A SOURCE GUARD, on the same rule as internal/cli's TestNoHardcodedTerraformExec
// and for the same reason: the defect is a call site reverting to the wrong
// helper, and the wrong helper produces a CORRECT result — just twice, with a
// duplicate report. Nothing observable from outside this package distinguishes
// them, so the behavioural half lives in tfbin (TestCommandResolvedDoesNotReResolve)
// and this half pins that the passthrough is the caller that uses it.
func TestPassthroughUsesTheAlreadyResolvedEnvironment(t *testing.T) {
	b, err := os.ReadFile("tofu.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "tfbin.CommandResolved(local.Environ()") {
		t.Error("tofu.go no longer execs through tfbin.CommandResolved with the environment it " +
			"already resolved")
	}
	if strings.Contains(src, "tfbin.Command(") {
		t.Error("tofu.go calls tfbin.Command, which resolves the instance environment AGAIN — " +
			"RunTofu has already done it for its own diagnostics and the backend key, so this " +
			"is a second walk of the filesystem and a second report of the same thing")
	}
}

// `--export` RUNS WITH NO SUBCOMMAND, so routing its encryption check through a
// predicate about OpenTofu VERBS answers false and skips the gate — after which
// the command exits 0 having handed the operator's shell an environment with no
// TF_ENCRYPTION in it. They eval it, believe they are configured, and meet
// "Invalid expression" on the next command: the failure this package exists to
// remove, reachable through the check meant to prevent it.
//
// The test sits at the CALL SITE deliberately. Mutating needsEncryption in
// isolation cannot catch this; a gate on a predicate is not a gate on its
// consumers.
func TestExportRefusesAnIncompleteEnvironment(t *testing.T) {
	instanceAt(t, "cluster", map[string]string{"TF_STATE_ACCESS_KEY": "ak"})

	var out, errBuf bytes.Buffer
	err := RunTofu(&out, &errBuf, TofuOpts{Export: true}, nil)
	if err == nil {
		t.Fatal("--export handed over an environment with no TF_ENCRYPTION and reported success; " +
			"the shell it sets up cannot run a single state-touching command")
	}
	if !strings.Contains(err.Error(), tfenc.PassphraseEnv) {
		t.Errorf("the refusal must name the secret to set, got: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("a refused --export still wrote %d bytes for a shell to eval: %q", out.Len(), out.String())
	}
}

// OpenTofu takes GLOBAL options before the subcommand (`tofu -chdir=DIR init`),
// so anchoring on args[0] reads `-chdir=…` as the verb. That made `--region` a
// silent no-op on precisely the invocation where the operator was being explicit
// about the directory.
func TestSubcommandIsFoundPastOpenTofuGlobalOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"init"}, "init"},
		{[]string{"-chdir=../vpc", "init"}, "init"},
		{[]string{"-chdir=../vpc", "fmt"}, "fmt"},
		{[]string{"-help"}, "help"},
		{[]string{"--version"}, "version"},
		{nil, ""},
		{[]string{"-chdir=x"}, ""},
	} {
		if got := subcommand(tc.args); got != tc.want {
			t.Errorf("subcommand(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
	// And the consumer: a global flag before an encryption-free verb must not
	// drag the whole invocation back into needing the key.
	if needsEncryption([]string{"-chdir=../vpc", "fmt"}) {
		t.Error("`-chdir=… fmt` was treated as needing the encryption config")
	}
	if !needsEncryption([]string{"-chdir=../vpc", "plan"}) {
		t.Error("`-chdir=… plan` must still require it")
	}
}

// --region derives the root from THIS directory while -chdir moves the one
// OpenTofu uses. Deriving anyway is the worst outcome available here and the
// tempting one: it would point an init at another root's state, silently. But
// --state-key names the key outright, so nothing is derived and it stays legal.
func TestChdirAndDerivedStateKeyCannotBeCombined(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	local, err := tfenc.Hydrate(".")
	if err != nil {
		t.Fatal(err)
	}

	_, err = withBackendConfig(TofuOpts{Region: "prod"}, local, []string{"-chdir=../vpc", "init"})
	if err == nil {
		t.Fatal("llz derived a state key from this directory for an init that runs in another one")
	}
	if !strings.Contains(err.Error(), "-chdir") || !strings.Contains(err.Error(), "--state-key") {
		t.Errorf("the refusal must name the conflict and the way through, got: %v", err)
	}

	got, err := withBackendConfig(TofuOpts{StateKey: "vpc/prod/terraform.tfstate"}, local,
		[]string{"-chdir=../vpc", "init"})
	if err != nil {
		t.Fatalf("--state-key names the key outright, so -chdir cannot invalidate it: %v", err)
	}
	if !contains(got, "-backend-config=key=vpc/prod/terraform.tfstate") {
		t.Errorf("want the explicit key applied, got %v", got)
	}

	// The silent no-op this replaced: --region with -chdir used to append nothing
	// and say nothing. Whatever happens now, it must not be that.
	if out, err := withBackendConfig(TofuOpts{Region: "prod"}, local, []string{"-chdir=x", "init"}); err == nil && len(out) == 2 {
		t.Error("--region was silently ignored rather than refused")
	}
}

// `--dry-run` is documented as "print commands; change nothing". A safety flag
// that silently does not work on the one command here that can destroy
// infrastructure is worse than no flag, because the operator relied on it.
func TestDryRunPrintsAndRunsNothing(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	stub := t.TempDir()
	ran := filepath.Join(stub, "ran")
	if err := os.WriteFile(filepath.Join(stub, "tofu"),
		[]byte("#!/bin/sh\ntouch "+ran+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF", filepath.Join(stub, "tofu"))

	var out, errBuf bytes.Buffer
	if err := RunTofu(&out, &errBuf, TofuOpts{DryRun: true}, []string{"apply", "-auto-approve"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("--dry-run EXECUTED the apply")
	}
	if !strings.Contains(errBuf.String(), "dry-run") || !strings.Contains(errBuf.String(), "apply -auto-approve") {
		t.Errorf("a dry run must show what would have run, got: %s", errBuf.String())
	}
	// A dry run is read by exactly the people who paste terminal output into
	// tickets, so it names the variables and never their values.
	for _, secret := range []string{testPassphrase, "sk", "pat"} {
		if strings.Contains(errBuf.String(), secret) {
			t.Errorf("the dry run printed a secret value:\n%s", errBuf.String())
		}
	}
	if !strings.Contains(errBuf.String(), tfenc.EnvVar) {
		t.Errorf("the dry run should name the variables it would set, got: %s", errBuf.String())
	}
}

// root.go's help says "Cloud-mutating commands execute only with --yes", and the
// extension declares cloud-mutate at `provisioned`. The declaration and the
// behaviour have to agree.
func TestMutatingVerbsNeedYes(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "tofu"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF", filepath.Join(stub, "tofu"))

	var out, errBuf bytes.Buffer
	err := RunTofu(&out, &errBuf, TofuOpts{}, []string{"apply", "-auto-approve"})
	if err == nil {
		t.Fatal("`apply -auto-approve` ran unattended with no --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal must name the flag, got: %v", err)
	}
	if err := RunTofu(&out, &errBuf, TofuOpts{Yes: true}, []string{"apply", "-auto-approve"}); err != nil {
		t.Fatalf("--yes should let it through: %v", err)
	}
	// Reading must stay frictionless, or the gate just teaches people to type
	// --yes reflexively — which is how a gate stops being one.
	for _, verb := range [][]string{{"plan"}, {"init"}, {"fmt"}, {"output"}, {"state", "list"}} {
		if err := RunTofu(&out, &errBuf, TofuOpts{}, verb); err != nil {
			t.Errorf("%v is read-only and must not need --yes: %v", verb, err)
		}
	}
}

// The danger in `state` and `workspace` lives in their SECOND word.
func TestStateSubVerbsAreResolvedOneTokenDeeper(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"state", "list"}, false},
		{[]string{"state", "show", "x"}, false},
		{[]string{"state", "rm", "x"}, true},
		{[]string{"state", "push", "f"}, true},
		{[]string{"state"}, true}, // a usage error; assume the worst
		{[]string{"workspace", "list"}, false},
		{[]string{"workspace", "delete", "x"}, true},
		{[]string{"some-future-verb"}, true}, // fails closed
	} {
		if got := mutates(tc.args); got != tc.want {
			t.Errorf("mutates(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// THE MODES WERE SEQUENTIAL EARLY RETURNS, so each silently swallowed the ones
// after it. `llz tofu --export -- plan` exported and never ran plan, exiting 0 —
// someone scripting `--export -- apply` got a green exit and no apply.
func TestModesRefuseToSwallowEachOther(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    TofuOpts
		args []string
		want string
	}{
		{"export with passthrough args", TofuOpts{Export: true}, []string{"plan"}, "silently dropped"},
		{"export with a backend flag", TofuOpts{Export: true, Region: "prod"}, nil, "--region"},
		{"shell-init with args", TofuOpts{ShellInit: true}, []string{"apply"}, "cannot also run"},
		{"shell-init with export", TofuOpts{ShellInit: true, Export: true}, nil, "Pick one"},
		{"shell-init with a backend flag", TofuOpts{ShellInit: true, Region: "prod"}, nil, "nothing to configure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			err := RunTofu(&out, &errBuf, tc.o, tc.args)
			if err == nil {
				t.Fatalf("accepted a contradictory invocation and did only part of it (stdout: %q)", out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want the refusal to mention %q, got: %v", tc.want, err)
			}
			if out.Len() != 0 {
				t.Errorf("a refused invocation still wrote to stdout: %q", out.String())
			}
		})
	}
}

// A DRY RUN MUST NOT WRITE THE PASSPHRASE TO DISK. `--export` writes a 0600 file
// carrying it, so the mode has to declare that and the shared policy has to cover
// it — which is what the effect/dispatch split is for.
func TestDryRunExportWritesNothing(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "llz-tofu-env-*"))

	var out, errBuf bytes.Buffer
	if err := RunTofu(&out, &errBuf, TofuOpts{Export: true, DryRun: true}, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "llz-tofu-env-*"))
	if len(after) != len(before) {
		for _, d := range after {
			_ = os.RemoveAll(d)
		}
		t.Fatalf("a dry run created %d export file(s) — each carrying the passphrase", len(after)-len(before))
	}
	if out.Len() != 0 {
		t.Errorf("a dry run emitted a snippet for a shell to source: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "dry-run") {
		t.Errorf("a dry run must say what it would have done, got: %s", errBuf.String())
	}
	for _, secret := range []string{testPassphrase, "sk", "pat"} {
		if strings.Contains(errBuf.String(), secret) {
			t.Errorf("the dry run printed a secret value:\n%s", errBuf.String())
		}
	}
}

// The rule behind readOnlySubVerbs — "the danger can live past the verb" — is
// equally true of init's FLAGS, and was applied to only one of the two shapes.
func TestFlagsCanMakeAReadOnlyVerbMutating(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"init"}, false},
		{[]string{"init", "-upgrade"}, false},
		{[]string{"init", "-backend-config=key=x"}, false},
		{[]string{"init", "-migrate-state"}, true},
		{[]string{"init", "-reconfigure"}, true},
		{[]string{"init", "-force-copy"}, true},
		{[]string{"-chdir=x", "init", "-migrate-state"}, true},
		{[]string{"plan", "-out=f"}, false},
	} {
		if got := mutates(tc.args); got != tc.want {
			t.Errorf("mutates(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// `--region` reaches OpenTofu because the PLANNED argv is what runs. The first
// cut of the mode split recomputed it in runExec from a zero TofuOpts, which
// silently dropped the backend config — a dry run would have shown one thing and
// the real run done another.
func TestPlannedArgvIsWhatRuns(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	local, err := tfenc.Hydrate(".")
	if err != nil {
		t.Fatal(err)
	}
	eff, err := plan(modeExec, TofuOpts{Region: "prod"}, local, []string{"init"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(eff.argv, "-backend-config=key=cluster/prod/terraform.tfstate") {
		t.Errorf("the planned argv lost --region: %v", eff.argv)
	}
	if !strings.Contains(eff.describe, "cluster/prod/terraform.tfstate") {
		t.Errorf("a dry run would describe something other than what runs: %q", eff.describe)
	}
}

// Every mode must pass the shared policy. A mode that could reach the world
// without being asked about --dry-run/--yes is the defect this structure exists
// to prevent, so the effect declaration is asserted directly.
func TestEveryEffectfulModeIsCoveredByThePolicy(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	local, _ := tfenc.Hydrate(".")

	exp, err := plan(modeExport, TofuOpts{}, local, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.writesLocal {
		t.Error("--export writes a 0600 file carrying the passphrase and must declare writesLocal, " +
			"or --dry-run will not cover it")
	}
	app, err := plan(modeExec, TofuOpts{}, local, []string{"apply"})
	if err != nil {
		t.Fatal(err)
	}
	if !app.mutatesInfra {
		t.Error("`apply` must declare mutatesInfra, or the --yes gate does not apply to it")
	}
}

// END TO END, because testing plan() alone leaves the caller free to ignore what
// it returns: recomputing the argv in runExec from a zero TofuOpts silently drops
// --region, and a mutation doing exactly that passes a plan()-only test. So this
// drives RunTofu and asks the STUB what OpenTofu actually received.
func TestRunTofuExecsThePlannedArgv(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	stub := t.TempDir()
	got := filepath.Join(stub, "argv")
	if err := os.WriteFile(filepath.Join(stub, "tofu"),
		[]byte("#!/bin/sh\necho \"$@\" > "+got+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF", filepath.Join(stub, "tofu"))

	var out, errBuf bytes.Buffer
	if err := RunTofu(&out, &errBuf, TofuOpts{Region: "prod"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("OpenTofu was never reached: %v", err)
	}
	if !strings.Contains(string(b), "-backend-config=key=cluster/prod/terraform.tfstate") {
		t.Errorf("--region did not survive the trip to OpenTofu; it received: %s", b)
	}
	if !strings.Contains(string(b), "-backend-config=bucket=state-bucket") {
		t.Errorf("the bucket did not survive; OpenTofu received: %s", b)
	}
}

// Ctrl-C reaches the whole process group, so llz gets a signal meant for
// OpenTofu; Go's default action kills llz while the child is still writing state.
//
// The test signals ITSELF: if the first interrupt is not absorbed, the default
// action terminates this test binary — a failure loud enough to not be mistaken
// for a pass.
func TestFirstInterruptIsLeftToTheChild(t *testing.T) {
	// syncBuffer, not bytes.Buffer: the note is written from ignoreInterrupts'
	// goroutine while this one reads it, which -race correctly flags. The real
	// caller passes os.Stderr and is safe for the reason documented there.
	var note syncBuffer
	restore := ignoreInterrupts(&note)
	defer restore()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("could not signal self: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // the signal is asynchronous
	// Reaching this line at all is the assertion that llz survived.

	// And the operator is told why nothing appeared to happen — without it, an
	// absorbed Ctrl-C is indistinguishable from a hang.
	if !strings.Contains(note.String(), "Ctrl-C again") {
		t.Errorf("an absorbed interrupt must say how to escalate, got: %q", note.String())
	}
}

// THE ESCAPE HATCH. Suppressing SIGINT outright leaves Ctrl-C doing nothing at
// all against a child that ignores the signal. A second interrupt means the
// operator has decided not to wait.
//
// escalateInterrupt is seamed because the alternative is a test that terminates
// the test binary to prove that it terminates.
func TestSecondInterruptAbandonsTheChild(t *testing.T) {
	escalated := make(chan struct{}, 1)
	prev := escalateInterrupt
	escalateInterrupt = func() { escalated <- struct{}{} }
	t.Cleanup(func() { escalateInterrupt = prev })

	restore := ignoreInterrupts(&syncBuffer{})
	defer restore()

	for i := 0; i < 2; i++ {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
			t.Fatalf("could not signal self: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-escalated:
	case <-time.After(2 * time.Second):
		t.Fatal("a second Ctrl-C did not restore the operator's ability to abandon the run — " +
			"against a child that ignores SIGINT this is an uninterruptible terminal")
	}
}

// RELEASING MUST ACTUALLY RELEASE, and the failure of dropping `done` is a
// GOROUTINE LEAK rather than a stray escalation: after signal.Stop nothing is
// delivered, so the goroutine blocks forever. Asserting "did not escalate" passes
// with the leak in place; counting goroutines distinguishes released from quiet.
func TestInterruptHandlingIsReleasedWithTheChild(t *testing.T) {
	// WARM UP FIRST: signal.Notify starts os/signal.loop once per process and never
	// stops it, so a baseline taken before any Notify can never be returned to.
	ignoreInterrupts(&syncBuffer{})()
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		ignoreInterrupts(&syncBuffer{})()
	}
	// Goroutine teardown is not synchronous with the channel close.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("ten install/release cycles leaked goroutines (%d -> %d) — one per Terraform "+
		"command llz drives, each blocked forever on a channel nothing will send to",
		before, runtime.NumGoroutine())
}

// withBackendConfig derives a command line; it must not write into the caller's.
// `append(args, …)` does exactly that whenever cobra's slice has spare capacity.
func TestBackendConfigDoesNotMutateTheCallersArgs(t *testing.T) {
	instanceAt(t, "cluster", fullSecrets())
	local, err := tfenc.Hydrate(".")
	if err != nil {
		t.Fatal(err)
	}
	// Spare capacity is the precondition for the aliasing bug, so construct it.
	args := make([]string, 1, 8)
	args[0] = "init"
	snapshot := append([]string(nil), args...)

	got, err := withBackendConfig(TofuOpts{Region: "prod"}, local, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != len(snapshot) || args[0] != snapshot[0] {
		t.Errorf("the caller's args were mutated: %v, want %v", args, snapshot)
	}
	// And the derived slice must not share the caller's backing array.
	if len(got) > 1 {
		got[0] = "CLOBBERED"
		if args[0] == "CLOBBERED" {
			t.Error("the returned argv aliases the caller's slice — writing to one writes to both")
		}
	}
}

// THE CALL SITE, not just the helper. ignoreInterrupts() passes its own test
// whether or not runExec ever calls it — a gate on a helper is not a gate on its
// consumer. Signals cannot be observed across the exec boundary from in-process,
// so this pins the wiring the way internal/cli's TestNoHardcodedTerraformExec
// does.
func TestPassthroughSuppressesInterruptsAroundTheChild(t *testing.T) {
	b, err := os.ReadFile("tofu.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	run := src[strings.Index(src, "func runExec("):]
	run = run[:strings.Index(run, "\n}\n")]
	if !strings.Contains(run, "ignoreInterrupts(") {
		t.Error("runExec no longer suppresses SIGINT around the child — Ctrl-C kills llz " +
			"instantly (measured: 0.00s) while `tofu apply` is still writing state, and the " +
			"operator gets a prompt back mid-write")
	}
	if !strings.Contains(run, "defer ignoreInterrupts(stderr)()") {
		t.Error("the suppression must be deferred so it is released when the child exits; " +
			"leaving it installed would make llz ignore Ctrl-C for the rest of its life")
	}
}

// syncBuffer is a bytes.Buffer safe for one writer and one reader, which is what
// ignoreInterrupts' contract requires of a caller passing a non-*os.File.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestUnrenderedRootIsRefusedRatherThanSilentlyDoingNothing is the gate for the
// one failure mode in this command that reports SUCCESS.
//
// A fresh instance clone has four empty root directories — the `*.tf` are
// generated by `llz render` and gitignored — and `tofu init` in an empty
// directory exits 0 with "OpenTofu initialized in an empty directory!". Every
// other refusal here is loud; this one used to be a green line and a lock that
// never changed, which is indistinguishable from success right up until the
// operator meets the same red check again.
//
// The first arm is the trap, the second is the false positive that would make the
// check worse than the trap: a rendered root must pass straight through.
func TestUnrenderedRootIsRefusedRatherThanSilentlyDoingNothing(t *testing.T) {
	t.Run("refuses, and names the command that renders", func(t *testing.T) {
		instanceAt(t, "cluster", fullSecrets())
		unrender(t)
		local, _ := tfenc.Hydrate(".")
		_, err := plan(modeExec, TofuOpts{}, local, []string{"init", "-upgrade"})
		if err == nil {
			t.Fatal("`llz tofu -- init` in an unrendered root was allowed through; " +
				"OpenTofu exits 0 there having written no lock, so this is a silent no-op")
		}
		for _, want := range []string{"llz render", "empty directory", "cluster"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must contain %q, got: %v", want, err)
			}
		}
		// THE COMMAND MUST BE RUNNABLE FROM WHERE IT IS PRINTED, which is inside the
		// root. `llz render` cannot be: instancelayout.Detect() is cwd-relative with
		// no upward walk, so from terraform-iac-bootstrap/cluster it looks for the
		// spec in that subdirectory and exits 1 with "no LandingZone spec found".
		// Asserting the substring `llz render` — which is all this did — passes on
		// the fragment that fails. Twice already in this release a gate over parts
		// passed while the composition was broken; this is the third place.
		if !strings.Contains(err.Error(), "cd ") {
			t.Errorf("the remedy must `cd` to the instance root — a bare `llz render` exits 1 "+
				"from inside a Terraform root, which is the only place this refusal fires. Got: %v", err)
		}
		root, dir, ok := strings.Cut(strings.SplitN(err.Error(), "(cd ", 2)[1], " && ")
		if !ok || dir == "" {
			t.Fatalf("could not read the cd target out of the remedy: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(root, clusterspec.LandingZoneFile)); statErr != nil {
			t.Errorf("the remedy `cd`s to %q, which holds no %s — so `llz render` would fail there too: %v",
				root, clusterspec.LandingZoneFile, statErr)
		}
	})

	t.Run("a rendered root passes through untouched", func(t *testing.T) {
		instanceAt(t, "cluster", fullSecrets())
		local, _ := tfenc.Hydrate(".")
		if _, err := plan(modeExec, TofuOpts{}, local, []string{"init", "-upgrade"}); err != nil {
			t.Fatalf("a rendered root must run: %v", err)
		}
	})

	// The scope limits, each of which exists to keep this from second-guessing an
	// operator who knows what they are doing.
	t.Run("says nothing outside a known root", func(t *testing.T) {
		instanceAt(t, "not-a-root", fullSecrets())
		unrender(t)
		local, _ := tfenc.Hydrate(".")
		if _, err := plan(modeExec, TofuOpts{}, local, []string{"plan"}); err != nil {
			t.Errorf("an empty directory that is not a Terraform root may be exactly what was meant: %v", err)
		}
	})

	t.Run("says nothing for verbs that do not read the configuration", func(t *testing.T) {
		instanceAt(t, "cluster", fullSecrets())
		unrender(t)
		local, _ := tfenc.Hydrate(".")
		for _, verb := range []string{"version", "fmt", "help"} {
			if _, err := plan(modeExec, TofuOpts{}, local, []string{verb}); err != nil {
				t.Errorf("`tofu %s` needs no configuration and must run anywhere: %v", verb, err)
			}
		}
	})

	// -chdir moves the directory OpenTofu operates in away from the one inspected
	// here, so refusing on THIS directory's contents would be a guess about another
	// one — the same refusal-to-guess withBackendConfig makes.
	t.Run("says nothing when -chdir moves the target directory", func(t *testing.T) {
		instanceAt(t, "cluster", fullSecrets())
		unrender(t)
		local, _ := tfenc.Hydrate(".")
		if _, err := plan(modeExec, TofuOpts{}, local, []string{"-chdir=../vpc", "plan"}); err != nil {
			t.Errorf("with -chdir this directory is not the one being operated on: %v", err)
		}
	})
}

// TestBackendFalseInitNeedsNoPassphrase holds the exemption that makes the
// provider-lock remedy runnable by the operator it is aimed at.
//
// `-backend=false` skips backend initialisation, which is the only thing the
// encryption block is consulted for. Demanding a passphrase for it refused
// exactly the person the flag exists to serve: someone who has never run
// `llz tokens`, blocked on a red provider-lock check, who only wants to
// regenerate a lock. Regenerating a lock never touches state.
//
// The negative arm is the one that keeps this honest — a plain `init` must still
// be refused, or the exemption has swallowed the tripwire ADR 0007 (state
// encryption) depends on.
func TestBackendFalseInitNeedsNoPassphrase(t *testing.T) {
	t.Run("regenerating a lock runs with no credentials at all", func(t *testing.T) {
		instanceAt(t, "cluster", map[string]string{}) // no passphrase, nothing
		local, _ := tfenc.Hydrate(".")
		if _, err := plan(modeExec, TofuOpts{}, local, []string{"init", "-backend=false", "-upgrade"}); err != nil {
			t.Fatalf("`init -backend=false` consults no encryption block and must not need a passphrase: %v", err)
		}
	})

	t.Run("a plain init is still refused", func(t *testing.T) {
		instanceAt(t, "cluster", map[string]string{})
		local, _ := tfenc.Hydrate(".")
		_, err := plan(modeExec, TofuOpts{}, local, []string{"init", "-upgrade"})
		if err == nil {
			t.Fatal("an init that configures the backend DOES need the passphrase — exempting it " +
				"would hand the operator OpenTofu's `Invalid expression` instead of a clear refusal")
		}
		if !strings.Contains(err.Error(), tfenc.PassphraseEnv) {
			t.Errorf("the refusal must still name the secret, got: %v", err)
		}
	})

	// The exemption is for `init` alone. `plan`/`apply` read state whether or not
	// anyone typed -backend=false, so the flag must not become a way past the gate.
	t.Run("the exemption does not leak to verbs that read state", func(t *testing.T) {
		instanceAt(t, "cluster", map[string]string{})
		local, _ := tfenc.Hydrate(".")
		for _, verb := range []string{"plan", "apply", "output"} {
			if _, err := plan(modeExec, TofuOpts{Yes: true}, local, []string{verb, "-backend=false"}); err == nil {
				t.Errorf("`tofu %s -backend=false` still reads state and must need the passphrase", verb)
			}
		}
	})

	// An unrendered root is refused BEFORE the passphrase question, so the operator
	// following the provider-lock remedy on a fresh clone is told to render rather
	// than told about a secret they do not need.
	t.Run("an unrendered root is still refused", func(t *testing.T) {
		instanceAt(t, "cluster", map[string]string{})
		unrender(t)
		local, _ := tfenc.Hydrate(".")
		_, err := plan(modeExec, TofuOpts{}, local, []string{"init", "-backend=false", "-upgrade"})
		if err == nil {
			t.Fatal("the exemption must not let an unrendered root through — that is the silent no-op")
		}
		if !strings.Contains(err.Error(), "llz render") {
			t.Errorf("the refusal must name `llz render`, got: %v", err)
		}
	})
}

// TestAnUnrenderedRootWithNoSpecIsNotSentToLlzRender closes the loop this PR is
// about, on the PR's own remedy.
//
// `llz render` needs a LandingZone spec and exits 1 without one. Naming it
// unconditionally meant a spec-less checkout was handed a second command that
// cannot run — the exact defect the provider-lock remedy was fixed for. Whether
// that combination is reachable in the field is not worth reasoning about when
// deciding it from the tree costs one file check.
func TestAnUnrenderedRootWithNoSpecIsNotSentToLlzRender(t *testing.T) {
	base := instanceAt(t, "cluster", fullSecrets())
	unrender(t)
	if err := os.Remove(filepath.Join(base, clusterspec.LandingZoneFile)); err != nil {
		t.Fatal(err)
	}
	local, _ := tfenc.Hydrate(".")
	_, err := plan(modeExec, TofuOpts{}, local, []string{"init", "-backend=false", "-upgrade"})
	if err == nil {
		t.Fatal("an unrendered root must still be refused whether or not there is a spec")
	}
	// The INSTRUCTION, not the mere mention: the spec-less message names `llz render`
	// too, to explain why it is not the answer. "Render them first" is the imperative.
	if strings.Contains(err.Error(), "Render them first") {
		t.Errorf("a spec-less checkout was told to run `llz render`, which refuses without a spec — "+
			"a remedy naming a command that cannot run:\n%v", err)
	}
	if !strings.Contains(err.Error(), clusterspec.LandingZoneFile) {
		t.Errorf("the refusal must say WHY render is not the answer here, got: %v", err)
	}
}
