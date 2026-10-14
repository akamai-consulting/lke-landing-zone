package evidencepack

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withKubectl(t *testing.T, fn func(args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlGet
	kubectlGet = fn
	t.Cleanup(func() { kubectlGet = orig })
}

func TestHarvestComplianceReport(t *testing.T) {
	withKubectl(t, func(args ...string) ([]byte, error) {
		if args[0] != "clustercompliancereports.aquasecurity.github.io" {
			t.Errorf("read %v, want the ClusterComplianceReport kind", args)
		}
		return []byte(`{"metadata":{"name":"cis"},"spec":{"compliance":{"id":"k8s-cis"}},
		  "status":{"summary":{"passCount":5,"failCount":1}}}`), nil
	})
	rep := harvestComplianceReport("cis")
	if rep == nil || rep.Name != "cis" || rep.FailCount != 1 {
		t.Fatalf("harvest = %+v, want cis/fail=1", rep)
	}

	withKubectl(t, func(...string) ([]byte, error) { return nil, errors.New("NotFound") })
	if rep := harvestComplianceReport("cis"); rep != nil {
		t.Errorf("harvest(error) = %+v, want nil (not collected)", rep)
	}
	withKubectl(t, func(...string) ([]byte, error) { return []byte("not json"), nil })
	if rep := harvestComplianceReport("cis"); rep != nil {
		t.Errorf("harvest(bad json) = %+v, want nil (not collected)", rep)
	}
}

func TestParseGates(t *testing.T) {
	g, err := parseGates([]string{"assert-rotation-health=success", "assert-wiz-enrollment=SKIPPED", "alert-eval=failure"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"assert-rotation-health": GatePass, "assert-wiz-enrollment": GateSkipped, "alert-eval": GateFail}
	for k, v := range want {
		if g[k] != v {
			t.Errorf("%s = %q, want %q", k, g[k], v)
		}
	}
	// A typo must not launder a failing gate out of the pack.
	for _, bad := range []string{"assert-rotation-health=FIAL", "=PASS", "no-equals"} {
		if _, err := parseGates([]string{bad}); err == nil {
			t.Errorf("parseGates(%q) accepted a malformed gate", bad)
		}
	}
}

func TestRunWritesPackAndFailsOnAFailedGate(t *testing.T) {
	withKubectl(t, func(...string) ([]byte, error) { return nil, errors.New("Unable to connect to the server") })
	origNow := now
	now = func() time.Time { return time.Unix(1700000000, 0) }
	t.Cleanup(func() { now = origNow })
	dir := t.TempDir()

	// Nothing collected, nothing recorded: NOT_COLLECTED — a warning, and a failure under --strict.
	if err := runCICISEvidence(cisEvidenceOpts{cluster: "c1", outDir: dir}); err != nil {
		t.Fatalf("non-strict empty pack must still be written, got %v", err)
	}
	if err := runCICISEvidence(cisEvidenceOpts{cluster: "c1", outDir: dir, strict: true}); err == nil {
		t.Fatal("--strict must fail a pack that examined nothing")
	}

	// A failed gate fails the run.
	if err := runCICISEvidence(cisEvidenceOpts{cluster: "c1", outDir: dir, gates: []string{"assert-rotation-health=FAIL"}}); err == nil {
		t.Fatal("a FAIL gate must fail the pack")
	}

	base := filepath.Join(dir, "cis-evidence-c1-1700000000")
	jb, err := os.ReadFile(base + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var p Pack
	if err := json.Unmarshal(jb, &p); err != nil {
		t.Fatal(err)
	}
	if p.Result != ResultFail || p.Gates["assert-rotation-health"] != GateFail || len(p.Questionnaire) != len(Questionnaire) {
		t.Errorf("pack = result %s gates %v questionnaire %d", p.Result, p.Gates, len(p.Questionnaire))
	}
	sum, err := os.ReadFile(base + ".json.sha256")
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(sum)), "cis-evidence-c1-1700000000.json") {
		t.Errorf("sha256 sidecar missing or malformed: %q %v", sum, err)
	}
	if _, err := os.Stat(base + ".md"); err != nil {
		t.Error("markdown summary not written")
	}
}

func TestPackRecord(t *testing.T) {
	p := BuildPack(Inputs{
		Cluster: "us-ord-primary", TimestampUnix: 1700000000,
		CISReport:    &ComplianceReport{Name: "cis", ID: "k8s-cis", PassCount: 9, FailCount: 1},
		Supplemental: Supplemental{NetworkPolicyCount: 7, RestrictedNamespaces: []string{"a", "b"}},
		Gates:        map[string]string{"b": GatePass, "a": GateSkipped},
	})
	if got := packBaseName(p); got != "cis-evidence-us-ord-primary-1700000000" {
		t.Errorf("packBaseName = %q", got)
	}
	rec := packRecord(p)
	if rec["result"] != ResultFail || rec["network_policy_count"] != 7 || rec["restricted_ns_count"] != 2 {
		t.Errorf("record wrong: %+v", rec)
	}
	if g, _ := rec["gates"].([]string); strings.Join(g, ",") != "a=SKIPPED,b=PASS" {
		t.Errorf("gates not sorted/flattened: %v", rec["gates"])
	}
}

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		t.Fatalf("cis-evidence does not validate: %v", errs)
	}
}
