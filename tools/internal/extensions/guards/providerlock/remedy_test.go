package providerlock

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfroots"
)

// TestTheRemedyIsTheCommandThatWorks is the behavior gate for this guard's
// entire operator-facing value.
//
// EVERY OTHER TEST HERE JUDGES A LOCK. This one judges the INSTRUCTION, which is
// the only thing an operator actually receives: a guard that detects the skew
// perfectly and then hands over a command that fails has moved the problem, not
// solved it. That is what shipped — the remedy said `tofu init -upgrade`, which
// stops on encryption.tf's tripwire in every instance checkout — and no test
// could see it, because no test ran the string.
//
// It runs the real command in a directory carrying the REAL embedded
// versions.tf and encryption.tf, and asserts the result satisfies THIS GUARD's
// own judgement (CheckRoot), not merely that a file changed. So the loop closes:
// the remedy the guard prints is proven to make the guard pass.
//
// ── WHY THE ROOT IS ASSEMBLED FROM TWO FILES INSTEAD OF RENDERED WHOLE ────────
//
// A rendered root's main.tf sources its modules over `git::ssh://` from a private
// repo, so `tofu init` would need an SSH agent and network access to a host no
// test may depend on. The two files written here are the only ones the property
// involves — versions.tf carries the constraint being satisfied and encryption.tf
// carries the tripwire being avoided — and both are the shipped bytes, so this
// cannot pass against a root that no longer looks like the one adopters get.
//
// Skips without OpenTofu, the same terms as tfenc's TestBuildSatisfiesTheShipped-
// Roots: the assertion needs a real init and there is nothing to substitute for
// one. It does reach the provider registry — regenerating a lock IS a resolution
// against it, so a hermetic version of this test would assert nothing.
func TestTheRemedyIsTheCommandThatWorks(t *testing.T) {
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("OpenTofu is required to prove the printed remedy regenerates a lock")
	}

	for root, versions := range tfroots.RootVersions() {
		constraints := ParseConstraints(versions, root+"/versions.tf")
		if len(constraints) == 0 {
			// Not a skip. A root whose versions.tf constrains nothing is a root this
			// guard can never fire on, and if that becomes true of ALL of them the
			// loop below would run zero assertions and still go green.
			t.Logf("%s: versions.tf constrains no provider — nothing for the remedy to fix", root)
			continue
		}

		t.Run(root, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "versions.tf"), versions)
			// THIS root's encryption.tf, not whichever one came out of the map first.
			// All four are byte-identical today, so picking arbitrarily passed — and
			// would have gone non-deterministically flaky the day one diverged, which
			// is the kind of failure that gets a test deleted rather than read.
			write(t, filepath.Join(dir, "encryption.tf"), shippedEncryptionTF(t, root))
			write(t, filepath.Join(dir, lockFile), stalePins(constraints))

			// The environment an operator meeting this remedy actually has: no
			// passphrase, no object-storage credentials, no bucket. That is the claim
			// -backend=false makes, so the test must not quietly supply any of them.
			bare := func(argv ...string) (string, error) {
				cmd := exec.Command(argv[0], argv[1:]...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "TF_ENCRYPTION=", "AWS_ACCESS_KEY_ID=",
					"AWS_SECRET_ACCESS_KEY=", "TF_STATE_BUCKET=")
				out, err := cmd.CombinedOutput()
				return string(out), err
			}

			// ── Arm 1: the OpenTofu the remedy runs must work ────────────────────
			// DERIVED FROM THE PRINTED STRING, not restated. Composing the command
			// out of parts and testing the parts is how `llz tofu -- tofu init …`
			// shipped: both halves were individually right and the string they made
			// was not runnable. tofuArgvOf strips the passthrough prefix off the
			// remedy an operator actually reads, so a malformed composition fails
			// here rather than in their terminal.
			out, err := bare(tofuArgvOf(t, RegenerateCmd)...)
			if err != nil {
				t.Fatalf("the remedy this guard prints FAILED — an operator following it stays stuck:\n"+
					"    %s\n%s", RegenerateCmd, out)
			}
			regenerated, err := os.ReadFile(filepath.Join(dir, lockFile))
			if err != nil {
				t.Fatalf("the remedy exited 0 but wrote no %s, so an operator would commit nothing "+
					"and meet this guard again: %v", lockFile, err)
			}
			// The guard's OWN verdict, so this cannot drift from what CI will say.
			if res := CheckRoot(root, constraints, ParseLock(string(regenerated))); len(res.Violations) > 0 {
				t.Errorf("the remedy ran clean and this guard still rejects the lock it produced:\n    %v", res.Violations)
			} else if res.Compared == 0 {
				t.Errorf("the regenerated lock matched no constraint — the remedy produced a %s this "+
					"guard cannot read, which is a pass over nothing:\n%s", lockFile, regenerated)
			}

			// ── Arm 2: -backend=false must be load-bearing ───────────────────────
			// Without this the flag could be dropped as noise and Arm 1 would keep
			// passing on any machine whose environment happened to carry a passphrase.
			write(t, filepath.Join(dir, lockFile), stalePins(constraints))
			if out, err := bare("tofu", "init", "-upgrade", "-input=false"); err == nil {
				t.Errorf("`tofu init -upgrade` SUCCEEDED without %s.\n"+
					"That is the remedy this guard used to print, and it working here means either the "+
					"tripwire is gone from encryption.tf or this test is not running the shipped one:\n%s",
					"TF_ENCRYPTION", out)
			}
		})
	}
}

// tofuArgvOf turns the printed remedy into the argv OpenTofu will really receive,
// by doing what `llz tofu` does: drop the passthrough prefix, run the rest.
//
// The prefix is REQUIRED rather than optional, so dropping the wrapper from the
// remedy fails here instead of quietly reverting this gate to testing a bare tofu.
func tofuArgvOf(t *testing.T, cmd string) []string {
	t.Helper()
	args, ok := strings.CutPrefix(cmd, LlzTofuPrefix)
	if !ok {
		t.Fatalf("the remedy must run OpenTofu through `llz tofu`, which refuses an unrendered root; "+
			"a bare `tofu` there exits 0 having written no lock. Got: %q", cmd)
	}
	if fields := strings.Fields(args); len(fields) == 0 || fields[0] == "tofu" {
		t.Fatalf("`llz tofu --` takes OpenTofu's ARGUMENTS, not its binary name — %q would run "+
			"`tofu tofu …`", cmd)
	}
	return append([]string{"tofu"}, strings.Fields(args)...)
}

// shippedEncryptionTF returns the encryption.tf the named root carries.
//
// FATAL RATHER THAN SKIPPED when it is absent: encryption.tf is what makes the
// naive remedy fail, so a root without one is a root where this guard's advice
// stopped mattering — that is a finding, not a reason to pass quietly.
func shippedEncryptionTF(t *testing.T, root string) string {
	t.Helper()
	want := filepath.Join("terraform-iac-bootstrap", root, "encryption.tf")
	for path, body := range tfroots.Files(".", "example-org", "v0.0.0") {
		if path == want {
			return body
		}
	}
	t.Fatalf("the %s root ships no encryption.tf — the state-at-rest tripwire this remedy has to "+
		"work around is gone there, and the remedy's shape should be revisited rather than this test skipped", root)
	return ""
}

// stalePins renders a lockfile pinning every constrained provider below anything
// the constraint can accept — the state an instance is in for the first commit
// after a release raises one.
//
// 0.0.1 rather than a real older release: it is below every constraint the roots
// have ever carried, needs no table of what each provider once shipped, and
// `-upgrade` re-resolves regardless of whether the pinned version exists.
func stalePins(constraints []Constraint) string {
	var b strings.Builder
	for _, c := range constraints {
		b.WriteString("provider \"registry.opentofu.org/" + c.Provider + "\" {\n" +
			"  version     = \"0.0.1\"\n  constraints = \"~> 0.0\"\n}\n\n")
	}
	return b.String()
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheRemedyRoutesThroughLlzTofu holds the wrapper, which is load-bearing and
// looks like decoration.
//
// A bare `tofu` in the printed remedy walks into the trap this same release
// documents: an instance commits zero `.tf` but DOES commit
// `.terraform.lock.hcl`, so a fresh clone's root directories hold a lock and
// nothing else — and `tofu init` there prints "OpenTofu initialized in an empty
// directory!", exits 0, and leaves the stale pin untouched. The operator's
// `git add` stages nothing and the guard fails again with no clue why. That is
// exactly the case quickstart §5 sends people to.
//
// `llz tofu` refuses an unrendered root and names `llz render`. Asserting the
// prefix is how that refusal stays reachable from this remedy — Arm 1 above
// executes the OpenTofu half directly and cannot see the wrapper at all.
func TestTheRemedyRoutesThroughLlzTofu(t *testing.T) {
	if !strings.HasPrefix(RegenerateCmd, "llz tofu -- ") {
		t.Errorf("the remedy must run OpenTofu through `llz tofu`, which refuses an unrendered "+
			"root; a bare `tofu` there exits 0 having written no lock. Got: %q", RegenerateCmd)
	}
	if !strings.Contains(RegenerateCmd, "-backend=false") {
		t.Errorf("the remedy must keep -backend=false — without it the init needs a passphrase "+
			"this operator may not have. Got: %q", RegenerateCmd)
	}
	// The two halves must not drift into describing different commands.
	if !strings.HasSuffix(RegenerateCmd, RegenerateTofuArgs) {
		t.Errorf("RegenerateCmd %q does not end in the OpenTofu args the gate executes (%q), so "+
			"the tested command and the printed one are different things", RegenerateCmd, RegenerateTofuArgs)
	}
}
