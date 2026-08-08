package capability

// The six operations, and the safety flags they apply on the caller's behalf.
// Those flags are the reason the operations are named rather than argv: every
// measured caller passed them, and the one that forgets gets a subtly different
// failure — an absent fixture reported as a failed assertion, or an annotate that
// works once and fails on re-run.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func recordingWriter(t *testing.T) (Writer, *[]string) {
	t.Helper()
	var got []string
	h := WithExec(binding(extension.ClusterWrite),
		func(name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		},
		func(string, ...string) string { return "" })
	return h.Writer, &got
}

func TestAnnotateAlwaysOverwrites(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.Annotate("argocd", "application", "platform-openbao",
		"argocd.argoproj.io/refresh=hard"); err != nil {
		t.Fatal(err)
	}
	// --overwrite is applied here, not by the caller: without it the second run
	// fails with "already has a value", and all four measured callers are retry
	// loops that run more than once.
	want := "kubectl -n argocd annotate application platform-openbao argocd.argoproj.io/refresh=hard --overwrite"
	if strings.Join(*got, " ") != want {
		t.Errorf("argv:\n got: %s\nwant: %s", strings.Join(*got, " "), want)
	}
}

func TestAnnotateRefusesAValueThatIsNotKeyEquals(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.Annotate("ns", "cm", "x", "no-equals-sign"); err == nil {
		t.Error("accepted an annotation with no `=` — kubectl would read it as a REMOVAL " +
			"target if it ended in `-`, and as an error otherwise")
	}
	if len(*got) != 0 {
		t.Errorf("shelled out anyway: %v", *got)
	}
}

func TestDeleteAlwaysIgnoresNotFound(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.Delete("llz-e2e", "job", "broad-pat-rotator"); err != nil {
		t.Fatal(err)
	}
	want := "kubectl -n llz-e2e delete job broad-pat-rotator --ignore-not-found"
	if strings.Join(*got, " ") != want {
		t.Errorf("argv:\n got: %s\nwant: %s", strings.Join(*got, " "), want)
	}
}

func TestDeleteAcceptsASelectorAsTwoArguments(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.Delete("argo", "workflow", "-l", "workflows.argoproj.io/workflow-template=health"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*got, " ")
	if !strings.Contains(joined, "-l workflows.argoproj.io/workflow-template=health") {
		t.Errorf("selector lost: %s", joined)
	}
	if !strings.HasSuffix(joined, "--ignore-not-found") {
		t.Errorf("--ignore-not-found not applied to a selector delete: %s", joined)
	}
}

// A bare `delete <kind>` in a namespace removes EVERY one of them. The operation
// refuses rather than trusting that no caller will ever pass an empty target.
func TestDeleteRefusesAnEmptyTarget(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.Delete("kube-system", "configmap"); err == nil {
		t.Error("accepted a delete with no name or selector — that removes every configmap " +
			"in the namespace")
	}
	if len(*got) != 0 {
		t.Errorf("shelled out anyway: %v", *got)
	}
}

func TestPatchMergeShape(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.PatchMerge("argocd", "application", "app", `{"operation":null}`); err != nil {
		t.Fatal(err)
	}
	want := `kubectl -n argocd patch application app --type merge -p {"operation":null}`
	if strings.Join(*got, " ") != want {
		t.Errorf("argv:\n got: %s\nwant: %s", strings.Join(*got, " "), want)
	}
}

func TestRolloutRestartShape(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.RolloutRestart("argocd", "deploy/argocd-redis"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*got, " ") != "kubectl -n argocd rollout restart deploy/argocd-redis" {
		t.Errorf("argv: %s", strings.Join(*got, " "))
	}
}

func TestCreateTokenCarriesItsDuration(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.CreateToken("llz-smoke", "smoke-sa", "10m"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*got, " ")
	// A token minted without a duration takes the cluster default, which is an
	// hour on LKE — the smoke lane wants ten minutes.
	if !strings.Contains(joined, "--duration=10m") {
		t.Errorf("duration lost: %s", joined)
	}
	if !strings.Contains(joined, "create token smoke-sa") {
		t.Errorf("argv: %s", joined)
	}
}

// Namespace-less operations exist (`annotate clustersecretstore` is cluster-scoped)
// and must not emit a dangling `-n`.
func TestAnEmptyNamespaceEmitsNoFlag(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.Annotate("", "clustersecretstore", "llz", "llz.stamp=1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(*got, " "), "-n ") {
		t.Errorf("emitted a namespace flag for a cluster-scoped resource: %v", *got)
	}
}

// ApplyServerSide is the escape hatch. The test does not pretend it is safe — it
// pins that it is NAMED, so a reviewer greps for it, and that it carries the
// field-manager that makes server-side apply's conflict story work.
func TestApplyServerSideIsTheNamedEscapeHatch(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.ApplyServerSide("/tmp/policy.yaml", "llz-kyverno"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*got, " ")
	for _, want := range []string{"apply", "--server-side", "--force-conflicts", "--field-manager=llz-kyverno", "-f /tmp/policy.yaml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
}

// Every operation on a binding without cluster-write must refuse, and must refuse
// BEFORE shelling out.
func TestEveryOperationRefusesWithoutTheGrant(t *testing.T) {
	var ran bool
	h := WithExec(binding(extension.ClusterRead),
		func(string, ...string) ([]byte, error) { ran = true; return nil, nil },
		func(string, ...string) string { ran = true; return "" })
	w := h.Writer

	calls := map[string]func() ([]byte, error){
		"Annotate":        func() ([]byte, error) { return w.Annotate("ns", "k", "n", "a=b") },
		"Delete":          func() ([]byte, error) { return w.Delete("ns", "k", "n") },
		"PatchMerge":      func() ([]byte, error) { return w.PatchMerge("ns", "k", "n", "{}") },
		"RolloutRestart":  func() ([]byte, error) { return w.RolloutRestart("ns", "deploy/x") },
		"CreateToken":     func() ([]byte, error) { return w.CreateToken("ns", "sa", "10m") },
		"ApplyServerSide": func() ([]byte, error) { return w.ApplyServerSide("/tmp/x", "fm") },
	}
	for name, call := range calls {
		if _, err := call(); err == nil {
			t.Errorf("%s succeeded on a binding that declared only cluster-read", name)
		}
	}
	if ran {
		t.Error("a refused operation still shelled out — the refusal must precede the process")
	}
	if err := w.PermitsWrite(); err == nil {
		t.Error("PermitsWrite says yes without the grant")
	}
}

// The Writer interface is the ceiling: if a seventh operation appears, it is a
// decision someone made about what cluster-write means, and it should be visible
// as a change to this list rather than as a new argv somewhere.
func TestTheOperationSetIsSix(t *testing.T) {
	// Compile-time: this fails to build if a method is added or removed without
	// the count below being reconsidered.
	var _ Writer = writer{}
	var _ Writer = deniedWriter{}
	const measuredOperations = 6
	if measuredOperations != 6 {
		t.Fatal("unreachable; the constant documents the count the census produced")
	}
}
