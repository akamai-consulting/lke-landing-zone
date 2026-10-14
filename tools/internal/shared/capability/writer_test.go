package capability

// The named operations, and the safety flags they apply on the caller's behalf.
// Those flags are the reason the operations are named rather than argv: every
// measured caller passed them, and the one that forgets gets a subtly different
// failure — an absent fixture reported as a failed assertion, or an annotate that
// works once and fails on re-run.

import (
	"os"
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
//
// THIS TEST USED TO BE VACUOUS, and it is worth recording because it is the reason
// the drift below survived a test whose NAME says it would not. It built a slice
// literal of eight strings and asserted `len(ops) != 8` — a tautology over a value
// it had just written, never once touching the Writer interface. A ninth operation
// could land and this would stay green; that is precisely what "six" surviving in
// five places looked like from here.
//
// It now asks the TYPE. See TestWriterOperationCountMatchesTheProse for the half
// that pins the header's number.
func TestTheOperationSetIsNine(t *testing.T) {
	var _ Writer = writer{}
	var _ Writer = deniedWriter{}
	want := []string{
		"Annotate", "ApplyServerSide", "ApplyStdin", "CreateStdin",
		"CreateToken", "Delete", "DeleteOrphan", "PatchMerge", "RolloutRestart",
	}
	got := WriterOperations()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Writer's operations are %v; this test expects %v. Adding one is a decision "+
			"about what cluster-write MEANS, so it should show up as a change here rather "+
			"than as a new argv somewhere.", got, want)
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

// TestWriterOperationCountMatchesTheProse pins the header's census against the
// interface.
//
// THE COUNT DRIFTED BY TWO AND FIVE SENTENCES CARRIED IT. `ApplyStdin` and
// `CreateStdin` were added to Writer with their own arguments recorded on each
// method, and the header above, two comments in capability.go, the Handles field
// doc and the refusal message all went on saying "six". Every one of them read
// correct on its own, which is why nothing looked.
//
// So the prose is checked rather than trusted. The refusal message is DERIVED from
// WriterOperations now, so it cannot drift at all; this covers the sentences that
// are genuinely prose and have to state a number to be worth reading.
func TestWriterOperationCountMatchesTheProse(t *testing.T) {
	ops := WriterOperations()
	if len(ops) != 9 {
		t.Fatalf("Writer offers %d operations (%v), and writer.go's header says nine. "+
			"An operation was added or removed without the census above it moving — update "+
			"both, because a header that miscounts the interface below it is how the "+
			"refusal message came to name six of eight.", len(ops), ops)
	}
	// PermitsWrite is the interrogation, not a mutation: a caller told to call it
	// "instead of assembling an argv" would be told to ask a question.
	for _, op := range ops {
		if op == "PermitsWrite" {
			t.Error("PermitsWrite is listed as a mutation a refused caller should use instead")
		}
	}
	if got := strings.Join(ops, ", "); !strings.Contains(got, "ApplyStdin") ||
		!strings.Contains(got, "CreateStdin") {
		t.Errorf("the derived operation list is %q — the two stdin operations are the ones "+
			"the seam-based census missed, so their absence here means the derivation broke, "+
			"not that they were removed", got)
	}
}

// THE PROSE AND THE COUNT DRIFTED APART ONCE ALREADY, in the direction that told
// a developer to use an operation that did not exist. TestWriterOperationCount
// MatchesTheProse pins the number; this pins the SENTENCES that quote it, which
// is where the last drift actually lived — the count was right in one place and
// stale in four others.
func TestTheHeaderProseNamesTheRightNumberOfShapes(t *testing.T) {
	src, err := os.ReadFile("writer.go")
	if err != nil {
		t.Fatal(err)
	}
	n := len(WriterOperations())
	word := map[int]string{8: "eight", 9: "nine", 10: "ten", 11: "eleven"}[n]
	if word == "" {
		t.Fatalf("Writer offers %d operations and this test has no word for it — add one, and check the "+
			"header says it", n)
	}
	for _, phrase := range []string{
		"these " + word + " shapes with these arguments",
		"Four of the " + word + " writers",
	} {
		if !strings.Contains(string(src), phrase) {
			t.Errorf("writer.go's header does not say %q — the interface has %d operations, and a header "+
				"that miscounts the code below it is how the last one came to advertise an operation that "+
				"was not there", phrase, n)
		}
	}
}

// safeArg refuses a leading `-`, not an empty string, so "" reached kubectl as a
// missing positional — `delete statefulset --cascade=orphan` in a namespace,
// which orphans every StatefulSet in it. Delete guards this; the operation with
// no undo did not.
func TestDeleteOrphanRefusesAnEmptyName(t *testing.T) {
	w, got := recordingWriter(t)
	if _, err := w.DeleteOrphan("monitoring", "statefulset", ""); err == nil {
		t.Fatal("an empty name must be refused: the argv it builds deletes every object of that kind")
	}
	if len(*got) != 0 {
		t.Errorf("nothing may be executed on the refused path, got %v", *got)
	}
	// …and a real name still works.
	if _, err := w.DeleteOrphan("monitoring", "statefulset", "loki-ingester"); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(*got, " "); !strings.Contains(joined, "--cascade=orphan") {
		t.Errorf("the orphan flag is the whole difference from Delete: %s", joined)
	}
}
