package assertidentity

// Tests moved here by the classify-then-split-by-line-range pass.

import (
	"strings"
	"testing"
	"time"
)

func TestEvalCertificates(t *testing.T) {
	now := time.Unix(1_720_000_000, 0).UTC()
	minLeft := 14 * 24 * time.Hour

	healthy := certItemJSON("llz-openbao", "openbao-tls", "True", now.Add(60*24*time.Hour).Format(time.RFC3339), "Ready")
	vs, err := evalCertificates(certListJSON(healthy), now, minLeft)
	if err != nil || vs[0].FailWhy != "" {
		t.Fatalf("a healthy cert must pass: %+v (%v)", vs, err)
	}

	// Broken ISSUANCE keeps cert-manager's own reason — that is the diagnosis.
	notReady := certItemJSON("ns", "bad", "False", "", "IssuerNotFound")
	vs, _ = evalCertificates(certListJSON(notReady), now, minLeft)
	if vs[0].FailWhy == "" || !strings.Contains(vs[0].FailWhy, "IssuerNotFound") {
		t.Errorf("a not-Ready cert must fail carrying the issuer's reason, got %q", vs[0].FailWhy)
	}

	// Broken RENEWAL is a DIFFERENT failure and must say so — the remedies differ.
	expiring := certItemJSON("ns", "soon", "True", now.Add(3*24*time.Hour).Format(time.RFC3339), "Ready")
	vs, _ = evalCertificates(certListJSON(expiring), now, minLeft)
	if vs[0].FailWhy == "" || !strings.Contains(vs[0].FailWhy, "renewal has stopped") {
		t.Errorf("an expiring cert must fail as a renewal problem, got %q", vs[0].FailWhy)
	}

	// Ready with no expiry is as blind as an expired one.
	noExpiry := certItemJSON("ns", "noexp", "True", "", "Ready")
	vs, _ = evalCertificates(certListJSON(noExpiry), now, minLeft)
	if vs[0].FailWhy == "" {
		t.Error("Ready with no notAfter must fail closed")
	}
	if _, err := evalCertificates([]byte(`nope`), now, minLeft); err == nil {
		t.Error("an unparseable list must be an error")
	}
}

// Zero Certificates is a failure: this platform issues its own CA chain, so none
// means cert-manager never reconciled — not that TLS is unused.
func TestRunAssertCertificatesFailsOnEmpty(t *testing.T) {
	orig := readCertificates
	t.Cleanup(func() { readCertificates = orig })
	readCertificates = func() ([]byte, error) { return []byte(`{"items":[]}`), nil }
	if err := runCIAssertCertificates(14, 0, time.Millisecond); err == nil {
		t.Error("no Certificates must fail rather than pass having examined nothing")
	}
}

func TestRunAssertCertificatesFailsOnNotReady(t *testing.T) {
	orig := readCertificates
	t.Cleanup(func() { readCertificates = orig })
	now := time.Now().UTC()
	readCertificates = func() ([]byte, error) {
		return certListJSON(
			certItemJSON("a", "good", "True", now.Add(90*24*time.Hour).Format(time.RFC3339), "Ready"),
			certItemJSON("b", "bad", "False", "", "Failed"),
		), nil
	}
	err := runCIAssertCertificates(14, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "b/bad") {
		t.Errorf("the failure must name the unhealthy certificate, got %v", err)
	}
}

// ── assert-database ──────────────────────────────────────────────────────────

// TestScramClientProofMatchesRFC7677 pins the SCRAM chain to the published
// vector rather than to whatever this implementation happens to compute. Without
// it, a subtly wrong proof would make every database look REJECTED and send
// someone rotating a password that was fine.
