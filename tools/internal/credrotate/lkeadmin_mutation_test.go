package credrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLKEAdminRotateWritesTheStepSummary pins the second half of the audit
// trail. The JSON record on stdout is consumed by the job log; the
// $GITHUB_STEP_SUMMARY block is what an auditor actually reads on the run page,
// and it is emitted AFTER the record is printed. Anything that short-circuits
// between the two leaves a rotation that happened with no durable evidence that
// it did — the summary silently empty while the command still exits 0.
func TestLKEAdminRotateWritesTheStepSummary(t *testing.T) {
	fake := &fakeLKEAdmin{k8sVersion: "v1.31.9+lke7"}
	withLKEAdmin(t, fake)
	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	t.Setenv("REGION", "us-ord")

	var runErr error
	out := captureStdout(t, func() { runErr = RunLKEAdminRotate(&Opts{Apply: true}, "4242") })
	if runErr != nil {
		t.Fatalf("apply: %v", runErr)
	}
	if !strings.Contains(out, `"event":"lke-admin-rotation"`) {
		t.Fatalf("the rotation record must still be printed on stdout:\n%s", out)
	}

	b, err := os.ReadFile(summary)
	if err != nil {
		t.Fatalf("$GITHUB_STEP_SUMMARY was never written — the run page carries no rotation evidence: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"### Rotation record — us-ord",
		"```json",
		`"event":"lke-admin-rotation"`,
		`"lke_cluster_id":4242`,
		`"api_action":"delete-kubeconfig (lke-admin)"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("step summary missing %q:\n%s", want, got)
		}
	}
}
