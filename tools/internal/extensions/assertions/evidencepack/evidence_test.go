package evidencepack

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseComplianceReport_SummaryAndControls(t *testing.T) {
	raw := []byte(`{
      "metadata": {"name": "cis"},
      "spec": {"compliance": {"id": "k8s-cis", "title": "CIS Kubernetes Benchmark"}},
      "status": {
        "summary": {"passCount": 40, "failCount": 3},
        "summaryReport": {"controls": [
          {"id": "5.2.1", "name": "no privileged", "severity": "HIGH", "totalFail": 2},
          {"id": "5.1.3", "name": "no wildcard", "severity": "HIGH", "totalFail": 0},
          {"id": "5.3.2", "name": "default-deny netpol", "severity": "MEDIUM", "totalFail": 1}
        ]}
      }
    }`)
	rep, err := ParseComplianceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Name != "cis" || rep.ID != "k8s-cis" {
		t.Errorf("metadata: got name=%q id=%q", rep.Name, rep.ID)
	}
	if rep.PassCount != 40 || rep.FailCount != 3 {
		t.Errorf("counts: got pass=%d fail=%d, want 40/3", rep.PassCount, rep.FailCount)
	}
	EvaluateReport(&rep, time.Now(), time.Hour)
	if rep.Result() != ResultFail {
		t.Errorf("result: got %q, want FAIL", rep.Result())
	}
	// Only the two controls with totalFail>0, sorted by id.
	if len(rep.FailingControls) != 2 {
		t.Fatalf("failing controls: got %d, want 2 (%+v)", len(rep.FailingControls), rep.FailingControls)
	}
	if rep.FailingControls[0].ID != "5.2.1" || rep.FailingControls[1].ID != "5.3.2" {
		t.Errorf("sort/filter: got %s,%s", rep.FailingControls[0].ID, rep.FailingControls[1].ID)
	}
}

func TestParseComplianceReport_DetailFallback(t *testing.T) {
	// No summary/summaryReport — must derive from detailReport.results[].
	raw := []byte(`{
      "metadata": {"name": "k8s-pss-restricted-0.1"},
      "spec": {"compliance": {"id": "k8s-pss-restricted-0.1", "title": "PSS restricted"}},
      "status": {
        "detailReport": {"results": [
          {"id": "XYZ-1", "name": "PSS", "severity": "HIGH", "status": "FAIL",
           "checks": [{"success": true}, {"success": false}, {"success": false}]},
          {"id": "XYZ-2", "name": "RBAC", "severity": "HIGH", "status": "PASS",
           "checks": [{"success": true}]}
        ]}
      }
    }`)
	rep, err := ParseComplianceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.PassCount != 1 || rep.FailCount != 1 {
		t.Errorf("derived counts: got pass=%d fail=%d, want 1/1", rep.PassCount, rep.FailCount)
	}
	if len(rep.FailingControls) != 1 || rep.FailingControls[0].ID != "XYZ-1" {
		t.Fatalf("failing controls: %+v", rep.FailingControls)
	}
	if rep.FailingControls[0].FailCount != 2 {
		t.Errorf("check failures: got %d, want 2", rep.FailingControls[0].FailCount)
	}
}

func TestParseComplianceReport_AllPass(t *testing.T) {
	raw := []byte(`{"metadata":{"name":"cis"},"spec":{"compliance":{"id":"k8s-cis"}},
      "status":{"updateTimestamp":"2023-11-14T22:00:00Z","summary":{"passCount":50,"failCount":0}}}`)
	rep, err := ParseComplianceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.UpdatedAt != "2023-11-14T22:00:00Z" {
		t.Errorf("updateTimestamp not read: %q", rep.UpdatedAt)
	}
	EvaluateReport(&rep, time.Date(2023, 11, 15, 0, 0, 0, 0, time.UTC), DefaultMaxReportAge)
	if rep.Result() != ResultPass {
		t.Errorf("result: got %q, want PASS", rep.Result())
	}
	if len(rep.FailingControls) != 0 {
		t.Errorf("failing controls: want none, got %+v", rep.FailingControls)
	}
}

func TestParseComplianceReport_BadJSON(t *testing.T) {
	if _, err := ParseComplianceReport([]byte("not json")); err == nil {
		t.Fatal("want error on malformed JSON")
	}
}

var t0 = time.Date(2023, 11, 15, 0, 0, 0, 0, time.UTC)

// THE HOLLOW-REPORT CASES. Each of these is a ClusterComplianceReport object that
// exists in the cluster and scored nothing — apl-core ships them with the
// compliance scanner off — and each must read NOT_COLLECTED, never PASS.
func TestEvaluateReportFailsClosed(t *testing.T) {
	fresh := t0.Add(-time.Hour).Format(time.RFC3339)
	cases := []struct {
		name string
		r    ComplianceReport
		want string
		why  string
	}{
		{"never reconciled (no updateTimestamp)", ComplianceReport{Name: "k8s-cis-1.23"}, ResultNotCollected, "never reconciled"},
		{"0 pass / 0 fail", ComplianceReport{Name: "k8s-cis-1.23", UpdatedAt: fresh}, ResultNotCollected, "scored nothing"},
		{"stale", ComplianceReport{Name: "k8s-cis-1.23", PassCount: 40, UpdatedAt: t0.Add(-48 * time.Hour).Format(time.RFC3339)}, ResultNotCollected, "stale"},
		{"garbled timestamp", ComplianceReport{Name: "k8s-cis-1.23", PassCount: 40, UpdatedAt: "yesterday"}, ResultNotCollected, "RFC 3339"},
		{"fresh and scored", ComplianceReport{Name: "k8s-cis-1.23", PassCount: 40, UpdatedAt: fresh}, ResultPass, ""},
		// A failure is never laundered by staleness or a missing timestamp.
		{"stale failure", ComplianceReport{Name: "k8s-cis-1.23", FailCount: 2, UpdatedAt: t0.Add(-48 * time.Hour).Format(time.RFC3339)}, ResultFail, ""},
		{"failure without timestamp", ComplianceReport{Name: "k8s-cis-1.23", FailCount: 2}, ResultFail, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.r
			EvaluateReport(&r, t0, DefaultMaxReportAge)
			if r.Result() != tc.want || !strings.Contains(r.Reason, tc.why) {
				t.Errorf("got %s (%q), want %s (%q)", r.Result(), r.Reason, tc.want, tc.why)
			}
		})
	}
	if (ComplianceReport{}).Result() != ResultNotCollected {
		t.Error("an unevaluated report must read NOT_COLLECTED")
	}
}

func TestBuildPack_Result(t *testing.T) {
	pass := ComplianceReport{Name: "k8s-cis-1.23", PassCount: 10, Status: ResultPass}
	fail := ComplianceReport{Name: "k8s-cis-1.23", FailCount: 1, Status: ResultFail}
	hollow := MissingReport("k8s-nsa-1.0", "never reconciled")
	on, off := ScannerEnabled, ScannerDisabled

	tests := []struct {
		name string
		in   Inputs
		want string
	}{
		// Nothing examined is not a pass — the vacuity arm.
		{"nothing collected", Inputs{}, ResultNotCollected},
		// Gate outcomes alone never make a pack PASS.
		{"only a passing gate", Inputs{Gates: map[string]string{"assert-rotation-health": GatePass}}, ResultNotCollected},
		{"scanner on, no reports expected", Inputs{ComplianceScanner: on}, ResultNotCollected},
		{"all reports pass, scanner on", Inputs{ComplianceScanner: on, Reports: []ComplianceReport{pass}}, ResultPass},
		// THE REVIEWED FAILURE: the reports exist, the scanner is off.
		{"reports pass but scanner off", Inputs{ComplianceScanner: off, Reports: []ComplianceReport{pass}}, ResultNotCollected},
		{"scanner state unknown", Inputs{Reports: []ComplianceReport{pass}}, ResultNotCollected},
		{"one report hollow", Inputs{ComplianceScanner: on, Reports: []ComplianceReport{pass, hollow}}, ResultNotCollected},
		{"report fail", Inputs{ComplianceScanner: on, Reports: []ComplianceReport{fail}}, ResultFail},
		{"report fail with scanner off", Inputs{ComplianceScanner: off, Reports: []ComplianceReport{fail}}, ResultFail},
		{"gate fail", Inputs{ComplianceScanner: on, Reports: []ComplianceReport{pass}, Gates: map[string]string{"assert-rotation-health": GateFail}}, ResultFail},
		{"gate pass", Inputs{ComplianceScanner: on, Reports: []ComplianceReport{pass}, Gates: map[string]string{"assert-rotation-health": GatePass}}, ResultPass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildPack(tc.in).Result; got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNotCollectedReasonNamesTheCause(t *testing.T) {
	off := BuildPack(Inputs{ComplianceScanner: ScannerDisabled})
	if !strings.Contains(NotCollectedReason(off), "OPERATOR_CLUSTER_COMPLIANCE_ENABLED") {
		t.Errorf("scanner-off reason = %q", NotCollectedReason(off))
	}
	hollow := BuildPack(Inputs{ComplianceScanner: ScannerEnabled, Reports: []ComplianceReport{MissingReport("k8s-nsa-1.0", "not readable: NotFound")}})
	if !strings.Contains(NotCollectedReason(hollow), "k8s-nsa-1.0: not readable") {
		t.Errorf("hollow reason = %q", NotCollectedReason(hollow))
	}
}

func TestBuildPack_FieldsAndOrder(t *testing.T) {
	a, b := MissingReport("k8s-cis-1.23", "x"), MissingReport("k8s-nsa-1.0", "y")
	p := BuildPack(Inputs{Cluster: "us-ord-primary", TimestampUnix: 1700000000, Reports: []ComplianceReport{a, b}})
	if p.Event != "cis-kubernetes-evidence" {
		t.Errorf("event: %q", p.Event)
	}
	if p.Cluster != "us-ord-primary" || p.ComplianceScanner != ScannerNotCollected {
		t.Errorf("cluster/scanner: %q %q", p.Cluster, p.ComplianceScanner)
	}
	if p.GeneratedAt == "" || !strings.HasPrefix(p.GeneratedAt, "2023-") {
		t.Errorf("generated_at not derived from timestamp: %q", p.GeneratedAt)
	}
	// An absent report stays IN the pack, so its absence is visible.
	if len(p.Reports) != 2 || p.Reports[0].Name != "k8s-cis-1.23" || p.Reports[1].Name != "k8s-nsa-1.0" {
		t.Errorf("reports order: %+v", p.Reports)
	}
}

func TestRenderMarkdown(t *testing.T) {
	// FAIL pack with a failing control + a not-collected supplemental signal.
	enc := true
	p := BuildPack(Inputs{
		Cluster:           "c1",
		TimestampUnix:     1700000000,
		ComplianceScanner: ScannerEnabled,
		Reports: []ComplianceReport{{
			Name: "k8s-cis-1.23", Title: "CIS Kubernetes Benchmarks v1.23", PassCount: 9, FailCount: 1, Status: ResultFail,
			FailingControls: []ControlResult{{ID: "5.2.1", Name: "no privileged", Severity: "HIGH", FailCount: 2}},
		}},
		Supplemental: Supplemental{
			NetworkPolicyCount:    -1, // not collected
			RestrictedNamespaces:  []string{"llz-openbao", "llz-observability"},
			EncryptedStorageClass: &enc,
		},
	})
	md := RenderMarkdown(p)
	for _, want := range []string{
		"# Compliance Evidence Pack",
		"❌ FAIL",
		"Failing controls",
		"5.2.1",
		"**Compliance scanner:** enabled",
		"⚠️ not collected", // network policy count -1
		"llz-openbao",
		"No questionnaire was provided",
		"Sign-Off",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestRenderMarkdown_ScannerOff(t *testing.T) {
	md := RenderMarkdown(BuildPack(Inputs{Cluster: "c1", TimestampUnix: 1700000000, ComplianceScanner: ScannerDisabled,
		Reports: []ComplianceReport{MissingReport("k8s-cis-1.23", "status.updateTimestamp is unset")}}))
	for _, want := range []string{"NOT COLLECTED", "compliance scanner is disabled", "status.updateTimestamp is unset"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestCountListItems(t *testing.T) {
	n, err := CountListItems([]byte(`{"items":[{"a":1},{"b":2},{"c":3}]}`))
	if err != nil || n != 3 {
		t.Errorf("got n=%d err=%v, want 3/nil", n, err)
	}
	n, err = CountListItems([]byte(`{"items":[]}`))
	if err != nil || n != 0 {
		t.Errorf("empty: got n=%d err=%v", n, err)
	}
}

func TestRestrictedNamespaces(t *testing.T) {
	raw := []byte(`{"items":[
      {"metadata":{"name":"llz-openbao","labels":{"pod-security.kubernetes.io/enforce":"restricted"}}},
      {"metadata":{"name":"default","labels":{"pod-security.kubernetes.io/enforce":"privileged"}}},
      {"metadata":{"name":"llz-observability","labels":{"pod-security.kubernetes.io/enforce":"restricted"}}},
      {"metadata":{"name":"kube-system","labels":{}}}
    ]}`)
	ns, err := RestrictedNamespaces(raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Sorted, only the two restricted ones.
	if len(ns) != 2 || ns[0] != "llz-observability" || ns[1] != "llz-openbao" {
		t.Errorf("got %v, want [llz-observability llz-openbao]", ns)
	}
}

func TestRestrictedNamespaces_EmptyIsNonNil(t *testing.T) {
	ns, err := RestrictedNamespaces([]byte(`{"items":[]}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ns == nil {
		t.Error("collected-but-empty must be non-nil to distinguish from not-collected")
	}
}

func TestDefaultStorageClassEncrypted(t *testing.T) {
	encrypted := []byte(`{"items":[
      {"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},
       "parameters":{"linodebs.csi.linode.com/encrypted":"true"}},
      {"metadata":{"annotations":{}},"parameters":{}}
    ]}`)
	got, err := DefaultStorageClassEncrypted(encrypted)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got == nil || !*got {
		t.Errorf("want encrypted=true, got %v", got)
	}

	plain := []byte(`{"items":[
      {"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},
       "parameters":{}}
    ]}`)
	got, err = DefaultStorageClassEncrypted(plain)
	if err != nil || got == nil || *got {
		t.Errorf("want encrypted=false, got %v err=%v", got, err)
	}

	noDefault := []byte(`{"items":[
      {"metadata":{"annotations":{}},"parameters":{"linodebs.csi.linode.com/encrypted":"true"}}
    ]}`)
	got, err = DefaultStorageClassEncrypted(noDefault)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("no default SC must yield nil (not collected), got %v", *got)
	}
}

func TestRenderMarkdown_CarriesTheAnsweredChecklist(t *testing.T) {
	q, err := ParseQuestionnaire([]byte(validQ+"  - {num: 3, title: \"Pipes | in text\", adopter: Do it.}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	md := RenderMarkdown(BuildPack(Inputs{
		Cluster: "c1", TimestampUnix: 1700000000, Questionnaire: q, QuestionnairePath: "infosec/questionnaire.yaml",
		Gates: map[string]string{"alert-eval": GatePass},
	}))
	for _, r := range q.Requirements {
		if !strings.Contains(md, fmt.Sprintf("| %d | %s |", r.Num, cell(r.Title))) {
			t.Errorf("pack markdown is missing checklist row %d (%s)", r.Num, r.Title)
		}
	}
	if !strings.Contains(md, `Pipes \| in text`) {
		t.Error("a pipe in an answer must be escaped, or it splits the table row")
	}
	if !strings.Contains(md, "`assert-rotation-health`: not run") {
		t.Error("a cited gate with no recorded outcome must say `not run`, not look passed")
	}
	if !strings.Contains(md, "my-org") {
		t.Error("the pack must name the questionnaire it carries")
	}
}

func TestComplianceScannerState(t *testing.T) {
	for raw, want := range map[string]string{
		`{"data":{"OPERATOR_CLUSTER_COMPLIANCE_ENABLED":"true"}}`:  ScannerEnabled,
		`{"data":{"OPERATOR_CLUSTER_COMPLIANCE_ENABLED":"false"}}`: ScannerDisabled,
		`{"data":{}}`: ScannerDisabled,
	} {
		if got, err := ComplianceScannerState([]byte(raw)); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", raw, got, err, want)
		}
	}
	if got, err := ComplianceScannerState([]byte("nope")); err == nil || got != ScannerNotCollected {
		t.Errorf("garbage = %q, %v", got, err)
	}
}
