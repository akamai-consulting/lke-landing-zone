package evidence

import (
	"strings"
	"testing"
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
      "metadata": {"name": "llz-org-controls"},
      "spec": {"compliance": {"id": "llz-org-controls", "title": "Org"}},
      "status": {
        "detailReport": {"results": [
          {"id": "SOG-9-PSS", "name": "PSS", "severity": "HIGH", "status": "FAIL",
           "checks": [{"success": true}, {"success": false}, {"success": false}]},
          {"id": "SOG-RBAC", "name": "RBAC", "severity": "HIGH", "status": "PASS",
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
	if len(rep.FailingControls) != 1 || rep.FailingControls[0].ID != "SOG-9-PSS" {
		t.Fatalf("failing controls: %+v", rep.FailingControls)
	}
	if rep.FailingControls[0].FailCount != 2 {
		t.Errorf("check failures: got %d, want 2", rep.FailingControls[0].FailCount)
	}
}

func TestParseComplianceReport_AllPass(t *testing.T) {
	raw := []byte(`{"metadata":{"name":"cis"},"spec":{"compliance":{"id":"k8s-cis"}},
      "status":{"summary":{"passCount":50,"failCount":0}}}`)
	rep, err := ParseComplianceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
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

func TestBuildPack_Result(t *testing.T) {
	pass := ComplianceReport{Name: "cis", PassCount: 10}
	fail := ComplianceReport{Name: "cis", FailCount: 1}

	tests := []struct {
		name string
		in   Inputs
		want string
	}{
		{"no reports collected -> PASS", Inputs{}, ResultPass},
		{"cis pass -> PASS", Inputs{CISReport: &pass}, ResultPass},
		{"cis fail -> FAIL", Inputs{CISReport: &fail}, ResultFail},
		{"org fail -> FAIL", Inputs{OrgReport: &fail}, ResultFail},
		{"cred audit fail -> FAIL", Inputs{CISReport: &pass, Supplemental: Supplemental{CredAuditResult: ResultFail}}, ResultFail},
		{"cred audit warnings -> PASS", Inputs{CISReport: &pass, Supplemental: Supplemental{CredAuditResult: "PASS_WITH_WARNINGS"}}, ResultPass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildPack(tc.in).Result; got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildPack_FieldsAndOrder(t *testing.T) {
	cis := ComplianceReport{Name: "cis"}
	org := ComplianceReport{Name: "llz-org-controls"}
	p := BuildPack(Inputs{Cluster: "us-ord-primary", TimestampUnix: 1700000000, CISReport: &cis, OrgReport: &org})
	if p.Event != "cis-kubernetes-evidence" {
		t.Errorf("event: %q", p.Event)
	}
	if p.Cluster != "us-ord-primary" {
		t.Errorf("cluster: %q", p.Cluster)
	}
	if p.GeneratedAt == "" || !strings.HasPrefix(p.GeneratedAt, "2023-") {
		t.Errorf("generated_at not derived from timestamp: %q", p.GeneratedAt)
	}
	if len(p.Reports) != 2 || p.Reports[0].Name != "cis" || p.Reports[1].Name != "llz-org-controls" {
		t.Errorf("reports order: %+v", p.Reports)
	}
}

func TestRenderMarkdown(t *testing.T) {
	// FAIL pack with a failing control + a not-collected supplemental signal.
	enc := true
	p := BuildPack(Inputs{
		Cluster:       "c1",
		TimestampUnix: 1700000000,
		CISReport: &ComplianceReport{
			Name: "cis", Title: "CIS Kubernetes Benchmark", PassCount: 9, FailCount: 1,
			FailingControls: []ControlResult{{ID: "5.2.1", Name: "no privileged", Severity: "HIGH", FailCount: 2}},
		},
		Supplemental: Supplemental{
			NetworkPolicyCount:    -1, // not collected
			RestrictedNamespaces:  []string{"llz-openbao", "llz-observability"},
			EncryptedStorageClass: &enc,
		},
	})
	md := RenderMarkdown(p)
	for _, want := range []string{
		"# CIS Kubernetes Benchmark — Evidence Pack",
		"❌ FAIL",
		"Failing controls",
		"5.2.1",
		"⚠️ not collected", // network policy count -1
		"llz-openbao",
		"Sign-Off",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestRenderMarkdown_NoReports(t *testing.T) {
	md := RenderMarkdown(BuildPack(Inputs{Cluster: "c1", TimestampUnix: 1700000000}))
	if !strings.Contains(md, "No ClusterComplianceReport was collected") {
		t.Errorf("expected no-reports warning, got:\n%s", md)
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
