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

// The Writer interface is the ceiling: a new operation is a decision someone made
// about what cluster-write means, and it should be visible as a change here rather
// than as a new argv somewhere.
//
// IT WAS SIX AND IS NOW EIGHT, which is the honest record of how it was built. The
// first census counted Deps seams and produced six shapes. It undercounted: every
// mutation that reached for exec.Command directly and piped a manifest to stdin was
// invisible to a seam-based count, and that is how assert-network came to be
// creating a namespace under an `assertion:verified[cluster-read]` declaration.
// ApplyStdin and CreateStdin are those two shapes.
func TestTheOperationSetIsEight(t *testing.T) {
	var _ Writer = writer{}
	var _ Writer = deniedWriter{}
	ops := []string{
		"Annotate", "Delete", "PatchMerge", "RolloutRestart",
		"CreateToken", "ApplyServerSide", "ApplyStdin", "CreateStdin",
	}
	if len(ops) != 8 {
		t.Fatalf("the operation list says %d; update the name of this test with it", len(ops))
	}
}

// The stdin operations exist because the FIRST census missed them: they were
// reached through raw exec.Command, so a seam-based count never saw them. Both
// route their manifest through the same stubbed process as everything else, which
// is the property worth pinning — an operation that escaped to a real kubectl
// because it happens to pipe its input would be exactly the old hole again.
func TestApplyStdinAndCreateStdin(t *testing.T) {
	w, got := recordingWriter(t)

	if _, err := w.ApplyStdin("apiVersion: v1\nkind: Namespace\n", "llz-net-probe"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*got, " ")
	for _, want := range []string{"apply", "--server-side", "--field-manager=llz-net-probe", "-f -"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ApplyStdin argv %q missing %q", joined, want)
		}
	}

	if _, err := w.CreateStdin("argo", "kind: Workflow\n"); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(*got, " ")
	// `create`, not `apply`: create FAILS on an existing object and the one caller
	// reads that failure as "a submission is already in flight".
	for _, want := range []string{"-n argo", "create", "-f -", "-o json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("CreateStdin argv %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "apply") {
		t.Errorf("CreateStdin used apply: %s", joined)
	}
}

func TestStdinOperationsRefuseWithoutTheGrant(t *testing.T) {
	var ran bool
	h := WithExec(binding(extension.ClusterRead),
		func(string, ...string) ([]byte, error) { ran = true; return nil, nil },
		func(string, ...string) string { ran = true; return "" })
	if _, err := h.Writer.ApplyStdin("kind: Namespace", "fm"); err == nil {
		t.Error("ApplyStdin succeeded on a cluster-read binding — this is the shape that " +
			"created a namespace undeclared for as long as it went through raw exec")
	}
	if _, err := h.Writer.CreateStdin("ns", "kind: Workflow"); err == nil {
		t.Error("CreateStdin succeeded on a cluster-read binding")
	}
	if ran {
		t.Error("a refused stdin operation still shelled out")
	}
}

func TestDeniedIsExportedAndRefuses(t *testing.T) {
	// Denied() exists so a struct whose Writer field was never populated degrades
	// to a refusal instead of a nil-pointer panic.
	if err := Denied().PermitsWrite(); err == nil {
		t.Error("Denied() permits writes")
	}
	if _, err := Denied().ApplyStdin("x", "y"); err == nil {
		t.Error("Denied().ApplyStdin succeeded")
	}
}
