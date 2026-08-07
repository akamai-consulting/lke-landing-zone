package main

// Coverage for the small pure helpers that had NO test at all.
//
// This file is deliberately narrow. A coverage sweep over cmd/llz found 183
// zero-coverage functions, and most of them should stay that way: they are the
// production halves of test seams (openHarborProvisionerBaoStore is one line
// delegating to openbao.OpenInClusterStore; ghOverlayRepo.ReadFile delegates to
// ghgitdata.ReadFile), thin exec wrappers (runTF builds a cmd, wires stdio, runs
// it), env readers (rotationInputsFromEnv is a struct literal of twelve
// os.Getenv calls), or cobra entrypoints whose decision logic is already
// extracted and tested. Covering those asserts wiring, not behaviour, and the
// seams are uncovered precisely BECAUSE the tests substitute them.
//
// What is left after removing those is this: pure functions where a wrong answer
// is silent. base64Auth with a swapped separator still returns valid base64 and
// breaks registry auth at runtime. isAlreadyExists misclassifying turns a benign
// conflict into a failure, or hides a real one. drivingEnabled dropping one
// disjunct silently disables a reconciler. Those are worth pinning; `clusterspec.OrDash` is
// borderline and included only because it costs nothing alongside its siblings.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harborauth"
)

// base64Auth builds the docker-config auth field. A swapped order or separator
// still produces valid base64, so the failure is a 401 at pull time rather than
// anything visible here — which is why the decoded form is asserted, not the
// encoded string.
func TestBase64Auth(t *testing.T) {
	got := base64Auth("robot$llz", "s3cr3t")
	if want := "cm9ib3QkbGx6OnMzY3IzdA=="; got != want {
		t.Errorf("base64Auth = %q, want %q", got, want)
	}
	// Decoding must yield exactly user:token — the separator is load-bearing and
	// a colon inside the token must not be mistaken for it.
	if got := base64Auth("u", "a:b"); got != "dTphOmI=" {
		t.Errorf("token containing a colon: got %q", got)
	}
	if base64Auth("a", "b") == base64Auth("b", "a") {
		t.Error("username and token are interchangeable — the order is not encoded")
	}
	if got := base64Auth("", ""); got != "Og==" { // ":" alone
		t.Errorf("empty credentials must still emit the separator, got %q", got)
	}
}

func TestTruncateForError(t *testing.T) {
	if got := harborauth.TruncateForError(nil); got != "(empty body)" {
		t.Errorf("nil body = %q", got)
	}
	if got := harborauth.TruncateForError([]byte("   \n\t ")); got != "(empty body)" {
		t.Errorf("whitespace-only body must read as empty, got %q", got)
	}
	if got := harborauth.TruncateForError([]byte("  boom  ")); got != "boom" {
		t.Errorf("body must be trimmed, got %q", got)
	}
	// The 200-byte boundary: exactly 200 is NOT truncated, 201 is.
	at := strings.Repeat("x", 200)
	if got := harborauth.TruncateForError([]byte(at)); got != at {
		t.Errorf("exactly 200 bytes must pass through untouched, got %d bytes", len(got))
	}
	over := strings.Repeat("x", 201)
	got := harborauth.TruncateForError([]byte(over))
	if got != strings.Repeat("x", 200)+"…" {
		t.Errorf("201 bytes must truncate to 200 plus an ellipsis, got %d bytes", len(got))
	}
	if strings.Count(got, "…") != 1 {
		t.Errorf("exactly one ellipsis expected, got %q", got)
	}
}

// cigate.EnvWithKubeconfig must REPLACE any inherited KUBECONFIG rather than append a
// second one — with two entries the child process takes the first, so an append
// would silently keep pointing at the operator's own cluster.
func TestEnvWithKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "/inherited/should/be/dropped")
	t.Setenv("LLZ_ENV_MARKER", "kept")

	env := cigate.EnvWithKubeconfig("/tmp/target.kubeconfig")

	var kubeconfigs []string
	marker := false
	for _, e := range env {
		if strings.HasPrefix(e, "KUBECONFIG=") {
			kubeconfigs = append(kubeconfigs, e)
		}
		if e == "LLZ_ENV_MARKER=kept" {
			marker = true
		}
	}
	if len(kubeconfigs) != 1 {
		t.Fatalf("want exactly one KUBECONFIG entry, got %v", kubeconfigs)
	}
	if kubeconfigs[0] != "KUBECONFIG=/tmp/target.kubeconfig" {
		t.Errorf("KUBECONFIG = %q, want the target path", kubeconfigs[0])
	}
	if !marker {
		t.Error("the rest of the environment must be preserved")
	}
}

// Moved BACK from internal/brownfield: clusterspec.OptSubnet lives in components_cmd.go and
// stayed. It travelled out because it shared a test file with brownfield symbols —
// the same "neighbours, not relatives" mistake the teardown extraction found, and
// the second time an iterative move has needed correcting.

func TestOptSubnetAndOrDash(t *testing.T) {
	if got := clusterspec.OptSubnet(""); got != "" {
		t.Errorf("clusterspec.OptSubnet(empty) = %q, want empty (no stray parens)", got)
	}
	if got := clusterspec.OptSubnet("10.0.0.0/24"); got != " (10.0.0.0/24)" {
		t.Errorf("clusterspec.OptSubnet = %q — the leading space separates it from the label", got)
	}
	if got := clusterspec.OrDash(""); got != "—" {
		t.Errorf("clusterspec.OrDash(empty) = %q, want an em dash", got)
	}
	if got := clusterspec.OrDash("us-ord"); got != "us-ord" {
		t.Errorf("clusterspec.OrDash passthrough = %q", got)
	}
}

func TestControlPlaneSummary(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		cp   clusterspec.ControlPlane
		want string
	}{
		{"nothing set", clusterspec.ControlPlane{}, ""},
		{"HA only", clusterspec.ControlPlane{HighAvailability: &yes}, "HA"},
		{"audit only", clusterspec.ControlPlane{AuditLogsEnabled: &yes}, "audit logs"},
		{"both", clusterspec.ControlPlane{HighAvailability: &yes, AuditLogsEnabled: &yes}, "HA, audit logs"},
		// Explicit false must read the same as unset: a pointer that is non-nil
		// but false is the case a plain nil-check would get wrong.
		{"explicit false", clusterspec.ControlPlane{HighAvailability: &no, AuditLogsEnabled: &no}, ""},
		{"HA false, audit true", clusterspec.ControlPlane{HighAvailability: &no, AuditLogsEnabled: &yes}, "audit logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterspec.ControlPlaneSummary(tc.cp); got != tc.want {
				t.Errorf("clusterspec.ControlPlaneSummary = %q, want %q", got, tc.want)
			}
		})
	}
}
