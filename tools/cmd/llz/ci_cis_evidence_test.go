package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/evidence"
)

func TestHarvestComplianceReport(t *testing.T) {
	// Happy path: kubectl returns a report, it parses.
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			t.Errorf("shelled out to %q, want kubectl", name)
		}
		return []byte(`{"metadata":{"name":"cis"},"spec":{"compliance":{"id":"k8s-cis"}},
		  "status":{"summary":{"passCount":5,"failCount":1}}}`), nil
	})
	rep := harvestComplianceReport("cis")
	if rep == nil || rep.Name != "cis" || rep.FailCount != 1 {
		t.Fatalf("harvest = %+v, want cis/fail=1", rep)
	}

	// Missing CRD / unreachable cluster -> nil (graceful).
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") })
	if rep := harvestComplianceReport("cis"); rep != nil {
		t.Errorf("harvest(error) = %+v, want nil", rep)
	}

	// Malformed JSON -> nil (graceful).
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("not json"), nil })
	if rep := harvestComplianceReport("cis"); rep != nil {
		t.Errorf("harvest(bad json) = %+v, want nil", rep)
	}
}

func TestReadCredAuditResult(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "cred.json")
	if err := os.WriteFile(good, []byte(`{"event":"linode-cred-audit","result":"PASS_WITH_WARNINGS"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := readCredAuditResult(good); err != nil || res != "PASS_WITH_WARNINGS" {
		t.Errorf("got (%q,%v), want PASS_WITH_WARNINGS", res, err)
	}

	// Captured cred-audit stdout: JSON record line + trailing human summary. Only
	// the first JSON value must be decoded.
	trailing := filepath.Join(dir, "trailing.json")
	os.WriteFile(trailing, []byte("{\"event\":\"linode-cred-audit\",\"result\":\"PASS\"}\nAll Linode PATs satisfy the policy.\n"), 0o644)
	if res, err := readCredAuditResult(trailing); err != nil || res != "PASS" {
		t.Errorf("trailing-text file: got (%q,%v), want PASS", res, err)
	}

	// Missing result field is an error.
	noField := filepath.Join(dir, "nofield.json")
	os.WriteFile(noField, []byte(`{"event":"x"}`), 0o644)
	if _, err := readCredAuditResult(noField); err == nil {
		t.Error("want error on missing result field")
	}

	// Missing file is an error.
	if _, err := readCredAuditResult(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("want error on missing file")
	}
}

func TestPackBaseNameAndRecord(t *testing.T) {
	p := evidence.BuildPack(evidence.Inputs{
		Cluster: "us-ord-primary", TimestampUnix: 1700000000,
		CISReport: &evidence.ComplianceReport{Name: "cis", ID: "k8s-cis", PassCount: 9, FailCount: 1},
		Supplemental: evidence.Supplemental{
			CredAuditResult: "PASS", NetworkPolicyCount: 7, RestrictedNamespaces: []string{"a", "b"},
		},
	})
	if got := packBaseName(p); got != "cis-evidence-us-ord-primary-1700000000" {
		t.Errorf("packBaseName = %q", got)
	}
	rec := packRecord(p)
	if rec["result"] != evidence.ResultFail {
		t.Errorf("record result = %v, want FAIL", rec["result"])
	}
	if rec["network_policy_count"] != 7 || rec["restricted_ns_count"] != 2 {
		t.Errorf("record counts wrong: %+v", rec)
	}
	reps, ok := rec["compliance_reports"].([]any)
	if !ok || len(reps) != 1 {
		t.Fatalf("record reports = %v", rec["compliance_reports"])
	}
}

func TestWritePack(t *testing.T) {
	dir := t.TempDir()
	p := evidence.BuildPack(evidence.Inputs{Cluster: "c1", TimestampUnix: 1700000000})
	if err := writePack(dir, p); err != nil {
		t.Fatalf("writePack: %v", err)
	}
	base := filepath.Join(dir, "cis-evidence-c1-1700000000")
	jb, err := os.ReadFile(base + ".json")
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	if !strings.Contains(string(jb), `"event": "cis-kubernetes-evidence"`) {
		t.Errorf("json missing event: %s", jb)
	}
	mb, err := os.ReadFile(base + ".md")
	if err != nil {
		t.Fatalf("read md: %v", err)
	}
	if !strings.Contains(string(mb), "Evidence Pack") {
		t.Errorf("md missing heading: %s", mb)
	}
}

func TestAttestBlobSkipsWithoutCosign(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	// No cosign on PATH -> no-op, no error (and must not shell out).
	withExecOutput(t, func(name string, _ ...string) ([]byte, error) {
		t.Errorf("attestBlob shelled out to %q with cosign absent", name)
		return nil, nil
	})
	if err := attestBlob("/tmp/pack.json", "/tmp/pack.json"); err != nil {
		t.Errorf("attestBlob(no cosign) = %v, want nil", err)
	}
}
