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

func withRepoFile(t *testing.T, files map[string]string) {
	t.Helper()
	orig := readRepoFile
	readRepoFile = func(rel string) ([]byte, error) {
		if b, ok := files[rel]; ok {
			return []byte(b), nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { readRepoFile = orig })
}

func withNow(t *testing.T, ts time.Time) {
	t.Helper()
	orig := now
	now = func() time.Time { return ts }
	t.Cleanup(func() { now = orig })
}

func TestHarvestComplianceReport(t *testing.T) {
	withNow(t, t0)
	withKubectl(t, func(args ...string) ([]byte, error) {
		if args[0] != "clustercompliancereports.aquasecurity.github.io" {
			t.Errorf("read %v, want the ClusterComplianceReport kind", args)
		}
		return []byte(`{"metadata":{"name":"k8s-cis-1.23"},"spec":{"compliance":{"id":"k8s-cis-1.23"}},
		  "status":{"updateTimestamp":"2023-11-14T23:00:00Z","summary":{"passCount":5,"failCount":1}}}`), nil
	})
	if rep := harvestComplianceReport("k8s-cis-1.23", DefaultMaxReportAge); rep.Name != "k8s-cis-1.23" || rep.Result() != ResultFail {
		t.Fatalf("harvest = %+v, want k8s-cis-1.23/FAIL", rep)
	}

	// The object apl-core ships with the scanner off: present, no status.
	withKubectl(t, func(...string) ([]byte, error) {
		return []byte(`{"metadata":{"name":"k8s-cis-1.23"},"spec":{"compliance":{"id":"k8s-cis-1.23","controls":[]}}}`), nil
	})
	if rep := harvestComplianceReport("k8s-cis-1.23", DefaultMaxReportAge); rep.Result() != ResultNotCollected {
		t.Errorf("a status-less report = %s, want NOT_COLLECTED", rep.Result())
	}

	withKubectl(t, func(...string) ([]byte, error) { return nil, errors.New("NotFound") })
	if rep := harvestComplianceReport("k8s-cis-1.23", DefaultMaxReportAge); rep.Result() != ResultNotCollected || rep.Name != "k8s-cis-1.23" || !strings.Contains(rep.Reason, "NotFound") {
		t.Errorf("harvest(error) = %+v, want a named NOT_COLLECTED entry", rep)
	}
	withKubectl(t, func(...string) ([]byte, error) { return []byte("not json"), nil })
	if rep := harvestComplianceReport("k8s-cis-1.23", DefaultMaxReportAge); rep.Result() != ResultNotCollected {
		t.Errorf("harvest(bad json) = %+v, want NOT_COLLECTED", rep)
	}
}

func TestHarvestScanner(t *testing.T) {
	withKubectl(t, func(args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "configmap trivy-operator-config -n trivy-operator -o json" {
			t.Errorf("read %v", args)
		}
		return []byte(`{"data":{"OPERATOR_CLUSTER_COMPLIANCE_ENABLED":"false"}}`), nil
	})
	if got := harvestScanner(); got != ScannerDisabled {
		t.Errorf("scanner = %q, want disabled", got)
	}
	withKubectl(t, func(...string) ([]byte, error) { return nil, errors.New("NotFound") })
	if got := harvestScanner(); got != ScannerNotCollected {
		t.Errorf("scanner(error) = %q, want not collected", got)
	}
}

func TestParseGates(t *testing.T) {
	g, err := parseGates([]string{"assert-rotation-health=success", "token-inventory=SKIPPED", "alert-eval=failure"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"assert-rotation-health": GatePass, "token-inventory": GateSkipped, "alert-eval": GateFail}
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

func TestRunWritesPackAndFailsClosed(t *testing.T) {
	withNow(t, time.Unix(1700000000, 0))
	withRepoFile(t, map[string]string{DefaultQuestionnairePath: validQ})
	dir := t.TempDir()
	base := opts(dir)

	// Unreachable cluster: NOT_COLLECTED — a warning, and a failure under --strict.
	withKubectl(t, func(...string) ([]byte, error) { return nil, errors.New("Unable to connect to the server") })
	if err := runCICISEvidence(base); err != nil {
		t.Fatalf("non-strict empty pack must still be written, got %v", err)
	}
	strict := base
	strict.strict = true
	if err := runCICISEvidence(strict); err == nil {
		t.Fatal("--strict must fail a pack that examined nothing")
	}

	// A failed gate fails the run.
	failing := base
	failing.gates = []string{"assert-rotation-health=FAIL"}
	if err := runCICISEvidence(failing); err == nil {
		t.Fatal("a FAIL gate must fail the pack")
	}

	p := readPack(t, dir)
	if p.Result != ResultFail || p.Gates["assert-rotation-health"] != GateFail || p.Questionnaire == nil || len(p.Questionnaire.Requirements) != 2 {
		t.Errorf("pack = result %s gates %v questionnaire %+v", p.Result, p.Gates, p.Questionnaire)
	}
	if len(p.Reports) != len(DefaultReports) {
		t.Errorf("every expected report must appear in the pack, collected or not: %d of %d", len(p.Reports), len(DefaultReports))
	}
	sum, err := os.ReadFile(filepath.Join(dir, "cis-evidence-c1-1700000000.json.sha256"))
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(sum)), "cis-evidence-c1-1700000000.json") {
		t.Errorf("sha256 sidecar missing or malformed: %q %v", sum, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cis-evidence-c1-1700000000.md")); err != nil {
		t.Error("markdown summary not written")
	}
}

// THE REVIEWED FAILURE, END TO END: the cluster holds apl-core's report objects
// and the scanner is off. Every report is present; nothing was scored. The pack
// must not PASS, and --strict must fail it — even with every gate green.
func TestRunWithTheScannerOffIsNotCollected(t *testing.T) {
	withNow(t, t0)
	withRepoFile(t, nil)
	withKubectl(t, func(args ...string) ([]byte, error) {
		switch args[0] {
		case "configmap":
			return []byte(`{"data":{"OPERATOR_CLUSTER_COMPLIANCE_ENABLED":"false"}}`), nil
		case "clustercompliancereports.aquasecurity.github.io":
			return []byte(`{"metadata":{"name":"` + args[1] + `"},"spec":{"compliance":{"id":"` + args[1] + `"}}}`), nil
		}
		return []byte(`{"items":[]}`), nil
	})
	dir := t.TempDir()
	o := opts(dir)
	o.gates = []string{"assert-rotation-health=success", "alert-eval=success", "token-inventory=success"}
	if err := runCICISEvidence(o); err != nil {
		t.Fatalf("non-strict: %v", err)
	}
	p := readPack(t, dir)
	if p.Result != ResultNotCollected || p.ComplianceScanner != ScannerDisabled {
		t.Errorf("result %s scanner %s, want NOT_COLLECTED/disabled", p.Result, p.ComplianceScanner)
	}
	if p.Questionnaire != nil {
		t.Error("no questionnaire file → the pack carries none")
	}
	o.strict = true
	if err := runCICISEvidence(o); err == nil || !strings.Contains(err.Error(), "OPERATOR_CLUSTER_COMPLIANCE_ENABLED") {
		t.Errorf("--strict with the scanner off = %v, want a failure naming the switch", err)
	}
}

func TestRunWithAScoredScanIsPass(t *testing.T) {
	withNow(t, t0)
	withRepoFile(t, nil)
	withKubectl(t, func(args ...string) ([]byte, error) {
		switch args[0] {
		case "configmap":
			return []byte(`{"data":{"OPERATOR_CLUSTER_COMPLIANCE_ENABLED":"true"}}`), nil
		case "clustercompliancereports.aquasecurity.github.io":
			return []byte(`{"metadata":{"name":"` + args[1] + `"},"status":{"updateTimestamp":"2023-11-14T23:00:00Z","summary":{"passCount":12,"failCount":0}}}`), nil
		}
		return []byte(`{"items":[]}`), nil
	})
	o := opts(t.TempDir())
	o.strict = true
	if err := runCICISEvidence(o); err != nil {
		t.Fatalf("a fresh, scored, passing scan must PASS under --strict: %v", err)
	}
}

func TestRunRefusesAnInvalidQuestionnaire(t *testing.T) {
	withRepoFile(t, map[string]string{DefaultQuestionnairePath: "kind: Nope\n"})
	withKubectl(t, func(...string) ([]byte, error) {
		t.Fatal("an invalid questionnaire must stop the run before the cluster is read")
		return nil, nil
	})
	if err := runCICISEvidence(opts(t.TempDir())); err == nil {
		t.Fatal("an invalid questionnaire must fail the pack")
	}
}

func TestCheckQuestionnaire(t *testing.T) {
	withKubectl(t, func(...string) ([]byte, error) {
		t.Fatal("--check-questionnaire must not read the cluster")
		return nil, nil
	})
	o := opts(t.TempDir())
	o.checkQuestionnaire = true
	withRepoFile(t, map[string]string{DefaultQuestionnairePath: validQ})
	if err := runCICISEvidence(o); err != nil {
		t.Fatalf("valid file: %v", err)
	}
	o.knownVerbs = map[string]bool{"alert-eval": true}
	if err := runCICISEvidence(o); err == nil {
		t.Error("a gate that is not a registered verb must fail the check")
	}
	withRepoFile(t, nil)
	o.knownVerbs = nil
	if err := runCICISEvidence(o); err == nil {
		t.Error("--check-questionnaire on a missing file must fail")
	}
}

func TestPackRecord(t *testing.T) {
	p := BuildPack(Inputs{
		Cluster: "us-ord-primary", TimestampUnix: 1700000000,
		Reports:      []ComplianceReport{{Name: "k8s-cis-1.23", ID: "k8s-cis-1.23", PassCount: 9, FailCount: 1, Status: ResultFail}},
		Supplemental: Supplemental{NetworkPolicyCount: 7, RestrictedNamespaces: []string{"a", "b"}},
		Gates:        map[string]string{"b": GatePass, "a": GateSkipped},
	})
	if got := packBaseName(p); got != "cis-evidence-us-ord-primary-1700000000" {
		t.Errorf("packBaseName = %q", got)
	}
	rec := packRecord(p)
	if rec["result"] != ResultFail || rec["network_policy_count"] != 7 || rec["restricted_ns_count"] != 2 || rec["compliance_scanner"] != ScannerNotCollected {
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

func opts(dir string) cisEvidenceOpts {
	return cisEvidenceOpts{cluster: "c1", outDir: dir, maxReportAge: DefaultMaxReportAge, questionnaire: DefaultQuestionnairePath}
}

func readPack(t *testing.T, dir string) Pack {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "cis-evidence-*.json"))
	if len(matches) == 0 {
		t.Fatal("no pack written")
	}
	jb, err := os.ReadFile(matches[len(matches)-1])
	if err != nil {
		t.Fatal(err)
	}
	var p Pack
	if err := json.Unmarshal(jb, &p); err != nil {
		t.Fatal(err)
	}
	return p
}
