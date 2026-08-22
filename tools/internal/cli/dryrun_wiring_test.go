package cli

// dryrun_wiring_test.go — --dry-run is only real if the COMMAND TREE delivers it.
//
// converge's nudge verb had a dry-run branch, a unit test proving that branch
// suppressed every exec, and a live cluster it mutated anyway. Nothing about the
// branch was wrong. The value reaching it was: installConvergeDeps copied
// cliopts.Global into converge.Deps while the tree was being BUILT, and cobra
// parses persistent flags when a command EXECUTES, so the copy was the zero
// value forever. `llz --dry-run ci nudge-argo` issued real annotate and patch
// writes against both Argo Applications.
//
// So this test refuses to call the verb's function. It builds the REAL root
// command, hands it the REAL argv an operator types, and asserts on what came
// out of the process seam — the only layer at which the freeze was visible.
// A unit test one level in cannot see it, which is precisely how it shipped.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
)

// runTreeCapturingExecs runs `llz <argv…>` against the real command tree with
// converge's process seam replaced, and returns every argv that escaped.
//
// The Writer is rebuilt through capability.WithExec rather than replaced with a
// no-op, so the mutations still travel their real path — binding selection,
// grant check, argv assembly — and are caught at the last possible moment. A
// stub Writer would prove the test's own no-op does nothing.
func runTreeCapturingExecs(t *testing.T, argv ...string) []string {
	t.Helper()
	var issued []string
	record := func(name string, args ...string) ([]byte, error) {
		issued = append(issued, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	recordComb := func(name string, args ...string) string {
		issued = append(issued, name+" "+strings.Join(args, " "))
		return ""
	}

	root := newRootCmd()

	// Re-install AFTER the tree is built, because building it is what installs
	// the real seams. Everything except the process seam stays as ci_converge.go
	// wired it.
	h := capability.WithExec(converge.Extension().MustBinding("drive"), record, recordComb)
	converge.Install(converge.Deps{
		Writer:                 h.Writer,
		Exec:                   record,
		ExecCombined:           recordComb,
		Summary:                func(string, ...string) error { return nil },
		FirewallDeploymentName: "x",
		FirewallConfigMapName:  "y",
	})
	t.Cleanup(func() { cliopts.Global = cliopts.Opts{} })

	root.SetArgs(argv)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("llz %s: %v", strings.Join(argv, " "), err)
	}
	return issued
}

// THE ARGV THE REVIEW RAN BY HAND. `--dry-run` before the subcommand, which is
// where a persistent flag goes and where it was being dropped.
func TestGlobalDryRunReachesTheNudgeVerb(t *testing.T) {
	issued := runTreeCapturingExecs(t, "--dry-run", "ci", "nudge-argo")
	if len(issued) != 0 {
		t.Errorf("`llz --dry-run ci nudge-argo` issued %d command(s) against the cluster:\n  %s",
			len(issued), strings.Join(issued, "\n  "))
	}
}

// AND AFTER THE SUBCOMMAND, which cobra accepts for a persistent flag and an
// operator will type. Two spellings of one flag is how a fix lands on the one
// that was tested.
func TestGlobalDryRunReachesTheNudgeVerbAfterTheSubcommand(t *testing.T) {
	issued := runTreeCapturingExecs(t, "ci", "nudge-argo", "--dry-run")
	if len(issued) != 0 {
		t.Errorf("`llz ci nudge-argo --dry-run` issued %d command(s) against the cluster:\n  %s",
			len(issued), strings.Join(issued, "\n  "))
	}
}

// WITHOUT THE FLAG IT MUST STILL MUTATE. A dry-run test alone passes just as
// happily against a verb that stopped doing anything at all, and "nudge-argo
// silently became a no-op" is a worse bug than the one being fixed — the
// post-seed convergence it drives would simply never happen.
func TestWithoutDryRunTheNudgeVerbStillMutates(t *testing.T) {
	issued := runTreeCapturingExecs(t, "ci", "nudge-argo")
	if len(issued) == 0 {
		t.Fatal("`llz ci nudge-argo` issued nothing — the verb is inert, which no dry-run test would notice")
	}
	joined := strings.Join(issued, "\n")
	for _, want := range []string{"annotate", "refresh=hard", "patch", "llz-secret-store"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the real run does not carry %q; the argv this gate compares against has moved:\n%s", want, joined)
		}
	}
}
