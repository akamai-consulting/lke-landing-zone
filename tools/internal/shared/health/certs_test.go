package health

import (
	"encoding/json"
	"testing"
)

func TestFindReady(t *testing.T) {
	const raw = `[
      {"type": "Synced", "status": "True"},
      {"type": "Ready", "status": "False", "reason": "IssuerNotReady", "message": "issuer platform-app-ca not ready"}
    ]`
	var conds []Condition
	if err := json.Unmarshal([]byte(raw), &conds); err != nil {
		t.Fatal(err)
	}
	status, reason, msg := FindReady(conds)
	if status != "False" || reason != "IssuerNotReady" || msg == "" {
		t.Errorf("FindReady = (%q,%q,%q)", status, reason, msg)
	}
	// No Ready condition -> Unknown.
	if s, _, _ := FindReady(nil); s != "Unknown" {
		t.Errorf("absent Ready should default to Unknown, got %q", s)
	}
}

func TestClassifyReady(t *testing.T) {
	// Ready=True passes.
	if cat, _ := ClassifyReady("ClusterIssuer", "platform-app-ca", "True", "", "", false, nil); cat != CatOK {
		t.Error("Ready=True should pass")
	}
	// phase1Pending routes to pending (caller resolves the condition).
	if cat, _ := ClassifyReady("ClusterIssuer", "platform-app-ca", "False", "IssuerNotReady", "", true, nil); cat != CatPending {
		t.Error("phase1Pending should be pending")
	}
	// operator-deferred match.
	dep := []DepEntry{{"some/secret", "external token not seeded"}}
	if cat, _ := ClassifyReady("ExternalSecret", "some/secret", "False", "SecretSyncedError", "", false, dep); cat != CatDeferred {
		t.Error("deferred match should defer")
	}
	// plain failure.
	if cat, _ := ClassifyReady("Certificate", "x/y", "False", "Failed", "boom", false, nil); cat != CatFail {
		t.Error("Ready=False with no routing should fail")
	}
}

func TestClassifyCertificateRequest(t *testing.T) {
	cases := []struct {
		name           string
		status, reason string
		phase1Pending  bool
		want           Category
	}{
		{"issued", "True", "Issued", false, CatOK},
		{"pending signature transient-ok", "False", "Pending", false, CatOK},
		{"phase1 wait", "False", "Pending", true, CatPending},
		{"hard failure", "False", "Failed", false, CatFail},
		{"denied", "False", "Denied", false, CatFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ClassifyCertificateRequest("ns/cr", c.status, c.reason, "msg", c.phase1Pending, nil)
			if got != c.want {
				t.Errorf("ClassifyCertificateRequest = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCertAllowlistsPresent(t *testing.T) {
	if len(Phase1PendingIssuers()) == 0 || len(Phase1PendingCerts()) == 0 {
		t.Error("phase-1 cert allowlists should be populated")
	}
}
