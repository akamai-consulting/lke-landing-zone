package main

// Coverage for the small pure helpers that had NO test at all.
//
// This file is deliberately narrow. A coverage sweep over cmd/llz found 183
// zero-coverage functions, and most of them should stay that way: they are the
// production halves of test seams (openHarborProvisionerBaoStore is one line
// delegating to openInClusterBaoStore; ghOverlayRepo.ReadFile delegates to
// ghReadFileNative), thin exec wrappers (runTF builds a cmd, wires stdio, runs
// it), env readers (rotationInputsFromEnv is a struct literal of twelve
// os.Getenv calls), or cobra entrypoints whose decision logic is already
// extracted and tested. Covering those asserts wiring, not behaviour, and the
// seams are uncovered precisely BECAUSE the tests substitute them.
//
// What is left after removing those is this: pure functions where a wrong answer
// is silent. base64Auth with a swapped separator still returns valid base64 and
// breaks registry auth at runtime. isAlreadyExists misclassifying turns a benign
// conflict into a failure, or hides a real one. drivingEnabled dropping one
// disjunct silently disables a reconciler. Those are worth pinning; `orDash` is
// borderline and included only because it costs nothing alongside its siblings.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
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
			if got := controlPlaneSummary(tc.cp); got != tc.want {
				t.Errorf("controlPlaneSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOptSubnetAndOrDash(t *testing.T) {
	if got := optSubnet(""); got != "" {
		t.Errorf("optSubnet(empty) = %q, want empty (no stray parens)", got)
	}
	if got := optSubnet("10.0.0.0/24"); got != " (10.0.0.0/24)" {
		t.Errorf("optSubnet = %q — the leading space separates it from the label", got)
	}
	if got := orDash(""); got != "—" {
		t.Errorf("orDash(empty) = %q, want an em dash", got)
	}
	if got := orDash("us-ord"); got != "us-ord" {
		t.Errorf("orDash passthrough = %q", got)
	}
}

func TestSuggestedInstanceDir(t *testing.T) {
	if got := suggestedInstanceDir(importReport{}); got != "lke-instance" {
		t.Errorf("no Linode section: got %q, want the fallback", got)
	}
	// A present-but-unlabelled cluster must fall back too — the nil check alone
	// is not enough, and an empty label would otherwise become an empty dirname.
	if got := suggestedInstanceDir(importReport{Linode: &importLinode{}}); got != "lke-instance" {
		t.Errorf("empty label: got %q, want the fallback", got)
	}
	if got := suggestedInstanceDir(importReport{Linode: &importLinode{Label: "prod-ord"}}); got != "prod-ord" {
		t.Errorf("labelled cluster: got %q", got)
	}
}

func TestVPCCIDRSummary(t *testing.T) {
	if got := vpcCIDRSummary(nil); got != "" {
		t.Errorf("nil VPC must be empty, got %q", got)
	}
	if got := vpcCIDRSummary(&lkeVPC{}); got != "" {
		t.Errorf("VPC with no subnets must be empty, got %q", got)
	}
	if got := vpcCIDRSummary(&lkeVPC{Subnets: []string{"10.0.0.0/24"}}); got != "10.0.0.0/24" {
		t.Errorf("single subnet: got %q (no trailing separator)", got)
	}
	if got := vpcCIDRSummary(&lkeVPC{Subnets: []string{"10.0.0.0/24", "10.0.1.0/24"}}); got != "10.0.0.0/24,10.0.1.0/24" {
		t.Errorf("multiple subnets: got %q", got)
	}
}

// poolsSuffix annotates a cluster line only when there is more than one pool, so
// the interesting input is exactly one — the boundary.
func TestPoolsSuffix(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, ""}, {1, ""}, {2, " (2 pools)"}, {5, " (5 pools)"}} {
		if got := poolsSuffix(make([]nodePool, tc.n)); got != tc.want {
			t.Errorf("poolsSuffix(%d pools) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFindCluster(t *testing.T) {
	clusters := []map[string]any{
		{"id": float64(101), "label": "a"},
		{"id": float64(202), "label": "b"},
	}
	got, ok := findCluster(clusters, 202)
	if !ok || got["label"] != "b" {
		t.Errorf("findCluster(202) = %v, %v — want the second cluster", got, ok)
	}
	if _, ok := findCluster(clusters, 999); ok {
		t.Error("a missing id must report not-found rather than the zero cluster")
	}
	if _, ok := findCluster(nil, 101); ok {
		t.Error("an empty list must report not-found")
	}
	// Returning the FIRST match matters: ids are unique upstream, but a
	// last-match-wins search would silently pick a different cluster if they
	// ever were not.
	dupes := []map[string]any{{"id": float64(7), "label": "first"}, {"id": float64(7), "label": "second"}}
	if got, _ := findCluster(dupes, 7); got["label"] != "first" {
		t.Errorf("duplicate ids: got %q, want the first match", got["label"])
	}
}

// lkeVPCInfo maps three fields out of an untyped API object. A transposed pair
// is invisible here and surfaces as a VPC attached to the wrong region.
func TestLKEVPCInfo(t *testing.T) {
	got := lkeVPCInfo(map[string]any{"id": float64(42), "label": "vpc-ord", "region": "us-ord"})
	if got.ID != 42 || got.Label != "vpc-ord" || got.Region != "us-ord" {
		t.Errorf("lkeVPCInfo = %+v — fields are transposed or dropped", got)
	}
	// Label and Region are both strings, so a swap type-checks; assert they are
	// not interchangeable.
	if got.Label == got.Region {
		t.Error("label and region resolved to the same value")
	}
	if empty := lkeVPCInfo(map[string]any{}); empty.ID != 0 || empty.Label != "" || empty.Region != "" {
		t.Errorf("missing keys must zero-value, got %+v", empty)
	}
}

func TestTruncateForError(t *testing.T) {
	if got := truncateForError(nil); got != "(empty body)" {
		t.Errorf("nil body = %q", got)
	}
	if got := truncateForError([]byte("   \n\t ")); got != "(empty body)" {
		t.Errorf("whitespace-only body must read as empty, got %q", got)
	}
	if got := truncateForError([]byte("  boom  ")); got != "boom" {
		t.Errorf("body must be trimmed, got %q", got)
	}
	// The 200-byte boundary: exactly 200 is NOT truncated, 201 is.
	at := strings.Repeat("x", 200)
	if got := truncateForError([]byte(at)); got != at {
		t.Errorf("exactly 200 bytes must pass through untouched, got %d bytes", len(got))
	}
	over := strings.Repeat("x", 201)
	got := truncateForError([]byte(over))
	if got != strings.Repeat("x", 200)+"…" {
		t.Errorf("201 bytes must truncate to 200 plus an ellipsis, got %d bytes", len(got))
	}
	if strings.Count(got, "…") != 1 {
		t.Errorf("exactly one ellipsis expected, got %q", got)
	}
}

// drivingEnabled is an eight-way OR. Dropping any single disjunct silently
// disables that reconciler's ability to keep the loop driving, so each flag is
// asserted to be sufficient ON ITS OWN.
func TestDrivingEnabled(t *testing.T) {
	if (reconcileOpts{}).drivingEnabled() {
		t.Error("no flags set must not drive")
	}
	// reconcileOpenBao and reconcileTokens are deliberately NOT in the
	// expression; if that changes, the last subtest here should start failing.
	for name, set := range map[string]func(*reconcileOpts){
		"argoNudge":  func(o *reconcileOpts) { o.reconcileArgoNudge = true },
		"cidrFW":     func(o *reconcileOpts) { o.reconcileCidrFW = true },
		"volLabels":  func(o *reconcileOpts) { o.reconcileVolLabels = true },
		"scDemote":   func(o *reconcileOpts) { o.reconcileSCDemote = true },
		"linodeCred": func(o *reconcileOpts) { o.reconcileLinodeCred = true },
		"harbor":     func(o *reconcileOpts) { o.reconcileHarbor = true },
		"esRecovery": func(o *reconcileOpts) { o.reconcileESRecovery = true },
		"aplOverlay": func(o *reconcileOpts) { o.reconcileAplOverlay = true },
	} {
		t.Run(name+" alone drives", func(t *testing.T) {
			var o reconcileOpts
			set(&o)
			if !o.drivingEnabled() {
				t.Errorf("%s alone must enable driving — it is a disjunct of drivingEnabled", name)
			}
		})
	}
	t.Run("openbao and tokens alone do not drive", func(t *testing.T) {
		o := reconcileOpts{reconcileOpenBao: true, reconcileTokens: true}
		if o.drivingEnabled() {
			t.Error("openbao/tokens are not driving reconcilers; if that changed, update drivingEnabled deliberately")
		}
	})
}

// isAlreadyExists decides whether a failed apply is benign. Both spellings occur:
// the Go-style API error and kubectl's prose.
func TestIsAlreadyExists(t *testing.T) {
	for _, s := range []string{
		`Error from server (AlreadyExists): configmaps "x" already exists`,
		"AlreadyExists",
		`Error from server: object already exists`,
	} {
		if !isAlreadyExists(s) {
			t.Errorf("must be treated as benign: %q", s)
		}
	}
	for _, s := range []string{
		"", "Error from server (Forbidden)",
		// Near-misses: neither spelling present. Case matters — a
		// case-insensitive match would wrongly swallow this one.
		"ALREADY EXISTS", "alreadyexists",
	} {
		if isAlreadyExists(s) {
			t.Errorf("must NOT be swallowed as benign: %q", s)
		}
	}
}

// validGlyph maps a validity status to a marker. NOTE the `case vWarn` arm and
// the `default` arm return the SAME value, so no test can distinguish them — the
// arm is redundant. What is assertable, and what matters, is that vInvalid is the
// only status rendered as a failure.
func TestValidGlyph(t *testing.T) {
	invalid := validGlyph(vInvalid)
	if !strings.Contains(invalid, "✗") {
		t.Errorf("vInvalid must render a cross, got %q", invalid)
	}
	for _, s := range []validityStatus{vWarn, vUnreachable} {
		got := validGlyph(s)
		if !strings.Contains(got, "⚠") {
			t.Errorf("status %v must render a warning, got %q", s, got)
		}
		if got == invalid {
			t.Errorf("status %v must not render identically to vInvalid", s)
		}
	}
}

// envWithKubeconfig must REPLACE any inherited KUBECONFIG rather than append a
// second one — with two entries the child process takes the first, so an append
// would silently keep pointing at the operator's own cluster.
func TestEnvWithKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "/inherited/should/be/dropped")
	t.Setenv("LLZ_ENV_MARKER", "kept")

	env := envWithKubeconfig("/tmp/target.kubeconfig")

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
