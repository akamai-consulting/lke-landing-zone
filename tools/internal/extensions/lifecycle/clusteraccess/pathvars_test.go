package clusteraccess

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestExpandPathVarsHonoursTheDocumentedContract(t *testing.T) {
	// Both action docstrings promise $HOME AND $RUNNER_TEMP expand. The first cut
	// of this repair did $HOME only, so a $RUNNER_TEMP path passed through as
	// literal text and the write landed in a directory of that name — with the
	// step still reporting available=true, which is the silent shape the whole
	// change exists to remove.
	env := fakeEnv(map[string]string{"HOME": "/home/runner", "RUNNER_TEMP": "/tmp/rt"})
	for in, want := range map[string]string{
		"$HOME/.kube/config":   "/home/runner/.kube/config",
		"${HOME}/.kube/config": "/home/runner/.kube/config",
		"$RUNNER_TEMP/kc":      "/tmp/rt/kc",
		"${RUNNER_TEMP}/kc":    "/tmp/rt/kc",
		"~/.kube/config":       "/home/runner/.kube/config",
		"~":                    "/home/runner",
		"/absolute/path":       "/absolute/path",
		"":                     "",
	} {
		if got := expandPathVars(in, env); got != want {
			t.Errorf("expandPathVars(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandPathVarsCannotExecuteItsInput(t *testing.T) {
	// THE POINT OF NOT USING `eval echo`, which is what the lke-runner-acl action
	// did. eval re-enters the parser, so this value would have RUN `id`. Here the
	// result is only ever text.
	env := fakeEnv(map[string]string{"HOME": "/home/runner"})
	got := expandPathVars(`$HOME/x";id;#`, env)
	if want := `/home/runner/x";id;#`; got != want {
		t.Fatalf("shell syntax must survive as inert text: got %q, want %q", got, want)
	}
}

func TestExpandPathVarsIsAClosedList(t *testing.T) {
	// os.ExpandEnv would expand EVERYTHING, letting a caller-supplied path read
	// arbitrary runner environment — GitHub exports secrets into it — and paste
	// the value into a filename. Only the three documented variables expand.
	env := fakeEnv(map[string]string{"HOME": "/home/runner", "GITHUB_TOKEN": "ghp_secret"})
	if got := expandPathVars("$HOME/$GITHUB_TOKEN", env); strings.Contains(got, "ghp_secret") {
		t.Fatalf("only the documented variables may expand, got %q", got)
	}
	// An unset documented variable leaves the reference visible rather than
	// collapsing the path to a silently-blank segment.
	if got := expandPathVars("$RUNNER_TEMP/kc", fakeEnv(map[string]string{})); got != "$RUNNER_TEMP/kc" {
		t.Fatalf("an unset variable must stay visible, got %q", got)
	}
}

func TestKubeconfigEnvExpandsOnlyKubeconfig(t *testing.T) {
	env := []string{"HOME=/home/runner", "KUBECONFIG=$HOME/.kube/config", "OTHER=$HOME/x"}
	got := kubeconfigExpandedEnv(env)
	var kube, other string
	for _, kv := range got {
		if n, v, ok := strings.Cut(kv, "="); ok {
			switch n {
			case "KUBECONFIG":
				kube = v
			case "OTHER":
				other = v
			}
		}
	}
	if kube != "/home/runner/.kube/config" {
		t.Errorf("KUBECONFIG must expand, got %q", kube)
	}
	if other != "$HOME/x" {
		t.Errorf("only KUBECONFIG is rewritten, got OTHER=%q", other)
	}
}

// reentry matches a delivered action handing a value BACK TO A PARSER. `eval` is
// the obvious primitive and the one actually there, but matching only that word
// let `sh -c "echo $VAR"` through — the identical RCE, different spelling,
// proven by putting it into the delivered action and watching this stay green.
//
// `-[a-zA-Z]*c` rather than `-c`: `bash -lc "$P"` and `sh -ec "$P"` are the same
// re-entry, and matching the bare flag let both through.
//
// The POSIX `.` spelling of `source` is DELIBERATELY ABSENT, and recorded rather
// than forgotten: `\. ` matches a bare `.` argument, which this tree writes in
// ordinary commands like `grep -rlE '…' . \`, and the `$` clause does not filter
// those because the same line often carries a `$(…)`. A matcher that fires on
// grep gets deleted, and then nothing checks anything. `source` is matched.
//
// A `$` must appear on the same line, and that is what keeps this precise rather
// than noisy: re-entering a parser over a LITERAL is ordinary shell (`bash -c
// 'set -e; make'`), while re-entering it over a variable is the hazard. Without
// that clause the first cut flagged `grep -rlZ 'x' .` and the words "from
// source".
var reentry = regexp.MustCompile("(^|[;&|(`]|\\s)(eval|(ba|z|k|da)?sh\\s+-[a-zA-Z]*c\\b|source\\s)")

func TestDeliveredActionsDoNotEvalAPath(t *testing.T) {
	// A COUPLING TEST, not a style rule. The expansion moved into Go precisely so
	// no delivered action has to reach for `eval`, and the one that did was
	// expanding a caller-supplied kubeconfig path — eval re-enters the parser, so
	// `$HOME/x";id;#` would have RUN. Put eval back and the Go half still passes;
	// nothing else in the tree would notice.
	// BOTH action trees, and .sh as well as .yml/.yaml. The first cut walked only
	// instance-template/.github/actions/*.yml, which let the eval move to
	// _lib/git-auth.sh — a file BOTH changed actions `source` — and pass.
	repo := filepath.Join("..", "..", "..", "..", "..")
	// WORKFLOWS TOO. The actions trees are not where the free-text inputs land —
	// llz-scheduled-checks.yml takes a `drift_branch`, so `eval echo "$DRIFT_BRANCH"`
	// there would pass both this test and the injection guard (the guard's remedy
	// IS routing through env:, and eval on an env var is invisible to it).
	roots := []string{
		filepath.Join(repo, "instance-template", ".github", "actions"),
		filepath.Join(repo, ".github", "actions"),
		filepath.Join(repo, "instance-template", ".github", "workflows"),
		filepath.Join(repo, ".github", "workflows"),
	}
	var offenders []string
	seen := 0
	perRoot := map[string]int{}
	currentRoot := ""
	walk := func(root string) error {
		currentRoot = root
		return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch filepath.Ext(p) {
			case ".yml", ".yaml", ".sh":
			default:
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			seen++
			perRoot[currentRoot]++
			for i, line := range strings.Split(string(b), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					continue // the comment recording why eval was removed
				}
				// WORD BOUNDARY, not a list of prefixes. The list version missed
				// `…; then eval …` and backtick `` `eval …` `` — proven by putting two
				// live re-entries back into the delivered action and watching this stay
				// green. Enumerating the shell syntaxes that can precede a command is a
				// losing game; asking whether the word appears at all is not.
				if reentry.MatchString(trimmed) && strings.Contains(trimmed, "$") {
					offenders = append(offenders, fmt.Sprintf("%s:%d: %s", p, i+1, trimmed))
				}
			}
			return nil
		})
	}
	for _, r := range roots {
		if err := walk(r); err != nil {
			t.Fatalf("walking %s: %v", r, err)
		}
	}
	// Fail closed: finding no actions is what a moved tree looks like, and this
	// check would then pass having read nothing.
	// PER ROOT, not an aggregate — the same defect guard.go's vacuity check was
	// rewritten to close. Summing across four trees means moving the delivered
	// actions alone still clears the threshold, so the test passes having read
	// zero of the files it exists for.
	for _, r := range roots {
		if perRoot[r] == 0 {
			// EXISTENCE IS NOT OPTIONAL HERE. Unlike the guard, this test only ever
			// runs from the template checkout, where all four trees are present — so
			// "absent" and "moved" are the same answer and both are failures. Gating
			// on os.Stat first reproduced the bug being fixed: moving the delivered
			// actions away made the root skip rather than fail.
			t.Fatalf("%s yielded no files — this check would be vacuous for it", r)
		}
	}
	if seen < 20 {
		t.Fatalf("expected the delivered actions and workflows, found %d — the check would be vacuous", seen)
	}
	if len(offenders) > 0 {
		t.Errorf("eval on a caller-supplied value in a delivered action:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// tfStub installs a fake `tofu` on PATH that emits a kubeconfig, mirroring
// TestFetchKubeconfigState's setup — the name must stay `tofu`, since a stub
// called `terraform` loses to a real tofu on the developer's PATH.
func tfStub(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	fake := "#!/bin/sh\nif [ \"$3\" = kubeconfig_raw ]; then printf 'apiVersion: v1'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "tofu"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestFetchFromStateWritesToTheExpandedPath(t *testing.T) {
	// THE GATE FOR THE REGRESSION ITSELF, at the level it actually happened.
	// Unit-testing expandPathVars proves the function works; it does not prove the
	// fetch path CALLS it, and deleting the call left every other test in this
	// package green. That is the same shape as the original bug: the write
	// succeeded, available=true was reported, and only the directory was wrong.
	d := testDeps(t)
	t.Setenv("TF_STATE_BUCKET", "tf-state")
	home := t.TempDir()
	t.Setenv("HOME", home)
	tfStub(t)

	prevInit := TfInitStream
	TfInitStream = func(args ...string) error { return nil }
	t.Cleanup(func() { TfInitStream = prevInit })

	// Exactly what all 11 callers pass: the literal text, unexpanded by GitHub.
	if err := RunFetchFromState(d, "primary", "$HOME/.kube/config", false); err != nil {
		t.Fatalf("fetch-kubeconfig-state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".kube", "config")); err != nil {
		t.Fatalf("kubeconfig must land at the expanded path: %v", err)
	}
	// And nothing may be created in a directory literally named `$HOME`, which is
	// where the regression put it while still reporting success.
	if _, err := os.Stat("$HOME"); err == nil {
		t.Error(`a directory literally named "$HOME" was created — the path was not expanded`)
		_ = os.RemoveAll("$HOME")
	}
}

func TestTheRealKubectlSeamRunsWithAnExpandedKubeconfig(t *testing.T) {
	// EXERCISES runnerACLKubectlFn ITSELF, not a stand-in. Every other test in this
	// package replaces that closure wholesale — which is how an unbounded kubectl
	// survived here once already, and how the first cut of the KUBECONFIG wiring
	// went ungated: deleting it left the package green, and because leaseOutcome
	// always returns nil the failure is silent. `runner-acl open` reports success,
	// the lease is never written, and the EAA controller evicts the runner IP
	// mid-job.
	//
	// So: a real fork/exec of a stub `kubectl` that reports the KUBECONFIG it was
	// handed, the same technique the tofu stub uses above.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", "$HOME/.kube/config")

	binDir := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s' \"$KUBECONFIG\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := runnerACLKubectlFn("", "get", "cm")
	if err != nil {
		t.Fatalf("stub kubectl: %v (%s)", err, got)
	}
	want := filepath.Join(home, ".kube", "config")
	if got != want {
		t.Fatalf("kubectl must receive the EXPANDED KUBECONFIG: got %q, want %q", got, want)
	}
}

func TestAnUnresolvablePathIsAnErrorNotAFilename(t *testing.T) {
	// The first cut left an unresolved reference in the path on the theory that
	// the text would show up in the error. There IS no error: both callers
	// MkdirAll the parent, write the file, and report available=true — so an unset
	// HOME produced a directory literally named `$HOME` and a green step, which is
	// the exact shape this change exists to delete.
	for _, p := range []string{"$HOME/.kube/config", "$RUNNER_TEMP/kc", "~/.kube/config", "${HOME}/x"} {
		if got, err := resolvePath(p, fakeEnv(map[string]string{})); err == nil {
			t.Errorf("resolvePath(%q) with nothing set must fail, got %q", p, got)
		}
	}
	// A resolvable path still resolves, and an absolute one is untouched.
	if got, err := resolvePath("$HOME/.kube/config", fakeEnv(map[string]string{"HOME": "/h"})); err != nil || got != "/h/.kube/config" {
		t.Errorf("resolvePath = %q, %v", got, err)
	}
	if got, err := resolvePath("/abs/path", fakeEnv(map[string]string{})); err != nil || got != "/abs/path" {
		t.Errorf("an absolute path must pass through: %q, %v", got, err)
	}
}

func TestFetchFromStateRefusesAnUnresolvablePath(t *testing.T) {
	// At the level it matters: the fetch must FAIL rather than write somewhere
	// wrong and report success.
	d := testDeps(t)
	t.Setenv("TF_STATE_BUCKET", "tf-state")
	t.Setenv("HOME", "")
	tfStub(t)
	prevInit := TfInitStream
	TfInitStream = func(args ...string) error { return nil }
	t.Cleanup(func() { TfInitStream = prevInit })

	err := RunFetchFromState(d, "primary", "$HOME/.kube/config", false)
	if err == nil {
		t.Fatal("an unresolvable --output must fail, not write to a directory named $HOME")
	}
	if !strings.Contains(err.Error(), "shell reference") {
		t.Errorf("the error must say why, got: %v", err)
	}
	if _, statErr := os.Stat("$HOME"); statErr == nil {
		t.Error(`a directory literally named "$HOME" was created`)
		_ = os.RemoveAll("$HOME")
	}
}

func TestVariableNamesHaveABoundary(t *testing.T) {
	// A plain ReplaceAll has no name boundary, so `$HOMEBREW_PREFIX` became
	// `/home/runnerBREW_PREFIX` — and because that result contains no `$`, it
	// sailed past the fail-closed check and got written to with available=true.
	// A mangled path is worse than a rejected one: it looks like it worked.
	env := fakeEnv(map[string]string{"HOME": "/home/runner", "RUNNER_TEMP": "/tmp/rt"})
	if got := expandPathVars("$HOMEBREW_PREFIX/kc", env); strings.HasPrefix(got, "/home/runner") {
		t.Errorf("$HOMEBREW_PREFIX is not $HOME, got %q", got)
	}
	if _, err := resolvePath("$HOMEBREW_PREFIX/kc", env); err == nil {
		t.Error("an unknown variable must be rejected, not mangled")
	}
	if _, err := resolvePath("$HOME_DIR/kc", env); err == nil {
		t.Error("$HOME_DIR is not $HOME")
	}
	// The real names still expand, including adjacent to other text.
	for in, want := range map[string]string{
		"$HOME/.kube/config": "/home/runner/.kube/config",
		"${HOME}x":           "/home/runnerx",
		"$RUNNER_TEMP/kc":    "/tmp/rt/kc",
	} {
		if got := expandPathVars(in, env); got != want {
			t.Errorf("expandPathVars(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGithubWorkspaceStillExpands(t *testing.T) {
	// The old shell interpolation expanded ANY variable, so narrowing to two names
	// would have turned a working custom kubeconfig-path into a hard error on
	// upgrade — a regression introduced by the fix for a regression.
	env := fakeEnv(map[string]string{"GITHUB_WORKSPACE": "/w"})
	got, err := resolvePath("$GITHUB_WORKSPACE/.kube/config", env)
	if err != nil || got != "/w/.kube/config" {
		t.Fatalf("resolvePath = %q, %v", got, err)
	}
}

func TestRunFetchResolvesItsOutputPathToo(t *testing.T) {
	// THE SIBLING CALL SITE. RunFetchFromState's resolvePath is gated end to end;
	// RunFetch's was not, so deleting it left the package green — the same shape
	// as the bug this whole change exists to fix, one call site over. The API path
	// is the one an instance uses when Terraform state is unavailable, so it is
	// not the lesser of the two.
	t.Setenv("LINODE_API_TOKEN", "")
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("HOME", "")
	// An unresolvable --output must be rejected BEFORE the token check, or the
	// operator fixes the token and then writes to a directory named $HOME.
	err := RunFetch(FetchOpts{Output: "$HOME/.kube/config"})
	if err == nil {
		t.Fatal("an unresolvable --output must fail")
	}
	if !strings.Contains(err.Error(), "shell reference") {
		t.Fatalf("the error must name the unresolved path, got: %v", err)
	}
}
