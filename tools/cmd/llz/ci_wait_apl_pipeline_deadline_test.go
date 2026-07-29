package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The KUBECONFIG_RAW guard had a test for the unset case, but nothing pinned it as
// a GUARD: with the check inverted, an unset KUBECONFIG_RAW simply falls through
// into the tempfile spill and then into a six-stage poll against a cluster that
// was never described — which is not a wrong answer, it is no answer at all, on a
// 55-minute budget.
//
// Both directions are checked with an unusable TMPDIR, so the step immediately
// after the guard has its own distinct, instant failure. That is what lets the two
// branches identify each other without a cluster and without a real poll.
func TestRunCIWaitAplPipelineKubeconfigGuardComesFirst(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "no-such-dir"))

	t.Setenv("KUBECONFIG_RAW", "")
	err := runCIWaitAplPipeline()
	if err == nil || !strings.Contains(err.Error(), "KUBECONFIG_RAW must be set") {
		t.Fatalf("err = %v, want the KUBECONFIG_RAW verdict — an unset kubeconfig has to be refused UP FRONT, not carried into the tempfile spill and the poll behind it", err)
	}

	t.Setenv("KUBECONFIG_RAW", "apiVersion: v1\nkind: Config\n")
	err = runCIWaitAplPipeline()
	if err == nil {
		t.Fatal("with an unwritable TMPDIR the kubeconfig spill must fail; a nil error means the guard let nothing through and nothing ran")
	}
	if strings.Contains(err.Error(), "KUBECONFIG_RAW must be set") {
		t.Fatalf("err = %v, want the tempfile failure — a SET KUBECONFIG_RAW must pass the guard", err)
	}
	if !strings.Contains(err.Error(), "create kubeconfig tempfile") {
		t.Fatalf("err = %v, want the tempfile-creation failure", err)
	}
}

// The existence poll's cadence: the stages exist to be waited for on a 10s beat
// (the bash `until kubectl get … sleep 10` this ports), and the existing tests
// reach their verdicts through a clock that moves on every now() READ — so they
// terminate whatever the loop waits, and never observe the interval.
//
// The CRD appears on the 4th probe, so this terminates on call count regardless.
func TestWaitForAplResourceExistencePollsOnATenSecondCadence(t *testing.T) {
	p := newPollRecorder()
	probes := 0
	d := p.deps(func(args ...string) (string, bool) {
		if strings.Contains(strings.Join(args, " "), "get crd/applications.argoproj.io") {
			probes++
			return "", probes >= 4
		}
		return "", true
	})
	st := aplWaitStage{
		desc: "Argo CD CRD", resource: "crd/applications.argoproj.io",
		forClause: "Established", existBudget: 10 * time.Minute, condTimeout: "3m",
	}
	if err := waitForAplResource(d, st); err != nil {
		t.Fatalf("the CRD appeared on the 4th probe but the stage failed: %v", err)
	}
	p.wantEveryPollAt(t, 10*time.Second, 3)
}

// The existence-timeout branch on an ADVANCING clock: a stage whose resource never
// appears must spend its budget at that cadence, then FAIL LOUD with diagnostics —
// the convergence contract this gate is written to keep.
func TestWaitForAplResourceExistenceDeadlineIsSpentAtThePollCadence(t *testing.T) {
	p := newPollRecorder()
	f := &fakeKubectl{responses: []kubectlRule{
		{match: "get crd/applications.argoproj.io", out: "", ok: false}, // never appears
		{match: "logs deploy/apl-operator", out: "helmfile: waiting on lock", ok: true},
	}}
	d := p.deps(f.run)
	st := aplWaitStage{
		desc: "Argo CD CRD", resource: "crd/applications.argoproj.io",
		forClause: "Established", existBudget: 30 * time.Second, condTimeout: "3m",
	}
	err := waitForAplResource(d, st)
	if err == nil || !strings.Contains(err.Error(), "crd/applications.argoproj.io did not appear within 30s") {
		t.Fatalf("err = %v, want the existence-timeout verdict naming the resource and the budget", err)
	}
	if f.called("wait --for") {
		t.Error("a resource that never appeared must not be handed to the condition wait")
	}
	if !f.called("logs deploy/apl-operator") {
		t.Error("an existence timeout must dump apl-operator logs — the stage exists to explain WHY the helmfile stalled")
	}
	// Probes at t=0,10,20,30; the one landing exactly on the deadline is the last
	// tried (pollUntil's !Before boundary), so three waits precede the verdict.
	p.wantEveryPollAt(t, 10*time.Second, 3)
	if got := p.elapsed(); got != 30*time.Second {
		t.Fatalf("clock advanced %s, want the full 30s budget", got)
	}
}
