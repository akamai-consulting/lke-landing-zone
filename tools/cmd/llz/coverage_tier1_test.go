package main

// Tier-1 coverage: table tests for the small PURE helpers that carried no direct
// test (they were only exercised incidentally through larger orchestrators).
// Each is deterministic on its inputs — no kubectl / API / filesystem.

import (
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"gopkg.in/yaml.v3"
)

func TestProgressingCondition(t *testing.T) {
	conds := []health.Condition{
		{Type: "Available", Reason: "MinimumReplicasAvailable"},
		{Type: "Progressing", Reason: "NewReplicaSetAvailable", Message: "rollout complete"},
	}
	if r, m := progressingCondition(conds); r != "NewReplicaSetAvailable" || m != "rollout complete" {
		t.Errorf("got (%q,%q), want (NewReplicaSetAvailable, rollout complete)", r, m)
	}
	if r, m := progressingCondition([]health.Condition{{Type: "Available"}}); r != "" || m != "" {
		t.Errorf("no Progressing condition should yield empty, got (%q,%q)", r, m)
	}
	if r, m := progressingCondition(nil); r != "" || m != "" {
		t.Errorf("nil conditions should yield empty, got (%q,%q)", r, m)
	}
}

func TestPrintHealthSummary(t *testing.T) {
	hardFail := captureStdout(t, func() {
		printHealthSummary(&health.Report{Failed: []string{"openbao sealed"}, Drift: []string{"argocd OutOfSync"}})
	})
	if !strings.Contains(hardFail, "FAILED:   openbao sealed") || !strings.Contains(hardFail, "1 check(s) hard-failed.") {
		t.Errorf("hard-fail summary wrong:\n%s", hardFail)
	}
	if !strings.Contains(hardFail, "drift:    argocd OutOfSync") {
		t.Errorf("drift line missing:\n%s", hardFail)
	}

	inProgress := captureStdout(t, func() {
		printHealthSummary(&health.Report{Pending: []string{"cert Issuing"}})
	})
	if !strings.Contains(inProgress, "still converging") {
		t.Errorf("in-progress summary wrong:\n%s", inProgress)
	}

	convergedDeferred := captureStdout(t, func() {
		printHealthSummary(&health.Report{Deferred: []string{"dns token"}})
	})
	if !strings.Contains(convergedDeferred, "1 operator-deferred item(s) remain") {
		t.Errorf("deferred-converged summary wrong:\n%s", convergedDeferred)
	}

	clean := captureStdout(t, func() { printHealthSummary(&health.Report{}) })
	if !strings.Contains(clean, "Cluster converged.") {
		t.Errorf("clean summary wrong:\n%s", clean)
	}
}

func TestSetScalarChild(t *testing.T) {
	m := &yaml.Node{Kind: yaml.MappingNode}
	setScalarChild(m, "count", "3")      // append int
	setScalarChild(m, "enabled", "true") // append bool
	setScalarChild(m, "count", "8")      // replace in place

	if len(m.Content) != 4 {
		t.Fatalf("expected 4 content nodes (2 keys), got %d", len(m.Content))
	}
	got := map[string]*yaml.Node{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		got[m.Content[i].Value] = m.Content[i+1]
	}
	if got["count"].Value != "8" || got["count"].Tag != "!!int" {
		t.Errorf("count = (%q,%q), want (8, !!int)", got["count"].Value, got["count"].Tag)
	}
	if got["enabled"].Value != "true" || got["enabled"].Tag != "!!bool" {
		t.Errorf("enabled = (%q,%q), want (true, !!bool)", got["enabled"].Value, got["enabled"].Tag)
	}
}

func TestCapitalizeFirst(t *testing.T) {
	for in, want := range map[string]string{"": "", "hello": "Hello", "A": "A", "123": "123"} {
		if got := capitalizeFirst(in); got != want {
			t.Errorf("capitalizeFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilepathRel(t *testing.T) {
	if got := filepathRel("/a/b/cluster", "/a/b/prod.tfvars"); got != "prod.tfvars" {
		t.Errorf("filepathRel = %q, want prod.tfvars", got)
	}
	// Unrelatable paths fall back to dst unchanged.
	if got := filepathRel("rel/dir", "/abs/out"); got != "/abs/out" {
		t.Errorf("filepathRel fallback = %q, want /abs/out", got)
	}
}

func TestInstanceLayout(t *testing.T) {
	t.Chdir(t.TempDir())

	// Rendered instance: roots at repo root.
	tf, apl, prefix := instanceLayout()
	if tf != "terraform-iac-bootstrap" || apl != "apl-values" || prefix != "" {
		t.Errorf("rendered layout = (%q,%q,%q)", tf, apl, prefix)
	}

	// Template-repo checkout: roots under instance-template/.
	if err := os.MkdirAll("instance-template/terraform-iac-bootstrap", 0o755); err != nil {
		t.Fatal(err)
	}
	tf, apl, prefix = instanceLayout()
	if tf != "instance-template/terraform-iac-bootstrap" || apl != "instance-template/apl-values" || prefix != "instance-template/" {
		t.Errorf("template layout = (%q,%q,%q)", tf, apl, prefix)
	}
}

func TestLiveStateValue(t *testing.T) {
	s := liveState{
		envVars:  map[string]string{"A": "env"},
		repoVars: map[string]string{"A": "repo", "B": "only-repo"},
	}
	if v := s.value("A"); v != "env" { // env scope wins
		t.Errorf("value(A) = %q, want env", v)
	}
	if v := s.value("B"); v != "only-repo" {
		t.Errorf("value(B) = %q, want only-repo", v)
	}
	if v := s.value("missing"); v != "" {
		t.Errorf("value(missing) = %q, want empty", v)
	}
}

func TestPaint(t *testing.T) {
	defer func(o bool) { colorOn = o }(colorOn)

	colorOn = false
	if got := paint("32", "x"); got != "x" {
		t.Errorf("paint with color off = %q, want x", got)
	}
	colorOn = true
	if got := paint("32", "x"); got != "\033[32mx\033[0m" {
		t.Errorf("paint with color on = %q", got)
	}
}

func TestEsPropFilesSortKey(t *testing.T) {
	if got := (esPropFiles{prop: "secret/x", hasProp: true}).sortKey(); got != "secret/x" {
		t.Errorf("sortKey(hasProp) = %q, want secret/x", got)
	}
	if got := (esPropFiles{prop: "secret/x", hasProp: false}).sortKey(); got != "" {
		t.Errorf("sortKey(!hasProp) = %q, want empty", got)
	}
}

func TestReadyCell(t *testing.T) {
	if got := readyCell(""); got != "Unknown" {
		t.Errorf("readyCell(\"\") = %q, want Unknown", got)
	}
	if got := readyCell("True"); got != "True" {
		t.Errorf("readyCell(True) = %q, want True", got)
	}
}

func TestSchedRegion(t *testing.T) {
	t.Setenv("REGION", "")
	if got := schedRegion(); got != "cluster" {
		t.Errorf("schedRegion unset = %q, want cluster", got)
	}
	t.Setenv("REGION", "us-ord")
	if got := schedRegion(); got != "us-ord" {
		t.Errorf("schedRegion set = %q, want us-ord", got)
	}
}
