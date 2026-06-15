package health

import "fmt"

// certs.go ports the "single Ready condition" check family — cert-manager
// ClusterIssuers/Certificates/CertificateRequests and ExternalSecrets/
// ClusterSecretStores — which all classify off a resource's Ready condition with
// the same Phase-1 / operator-deferred routing. The only variants are how the
// caller resolves phase1Pending (a name-match list for Issuers/Certs, plain
// phase1 for ES/CSS) and the CertificateRequest "Pending is transient" branch.

// Condition is one entry of a resource's .status.conditions.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// FindReady returns the Ready condition's status/reason/message, defaulting
// status to "Unknown" when no Ready condition is present (jq `// "Unknown"`).
func FindReady(conds []Condition) (status, reason, message string) {
	for _, c := range conds {
		if c.Type == "Ready" {
			return c.Status, c.Reason, c.Message
		}
	}
	return "Unknown", "", ""
}

// ClassifyReady classifies a resource with a single Ready condition. Ready=True
// passes; otherwise — when phase1Pending (the caller resolves this: a name match
// for Issuers/Certs, plain Phase 1 for ES/CSS) — it's a pending OpenBao-bootstrap
// wait; else an operator-deferred match defers it; else it fails.
func ClassifyReady(kind, key, status, reason, msg string, phase1Pending bool, extDep []DepEntry) (Category, string) {
	if status == "True" {
		return CatOK, fmt.Sprintf("%s %s Ready", kind, key)
	}
	detail := fmt.Sprintf("%s %s (Ready=%s reason=%s)", kind, key, status, reason)
	if phase1Pending {
		return CatPending, detail + " — waiting on OpenBao bootstrap"
	}
	if r, ok := MatchExternalDep(key, extDep); ok {
		return CatDeferred, detail + " — " + r
	}
	return CatFail, fmt.Sprintf("%s %s (Ready=%s reason=%s message=%s)", kind, key, status, reason, msg)
}

// ClassifyCertificateRequest is ClassifyReady for a CertificateRequest, with one
// extra branch: a Ready=False whose reason is "Pending" is transient-OK (the CR
// is just waiting on the issuer to sign a fresh request) rather than a failure.
// Failed/Denied/InvalidRequest reasons still fail.
func ClassifyCertificateRequest(key, status, reason, msg string, phase1Pending bool, extDep []DepEntry) (Category, string) {
	if status == "True" {
		return CatOK, "CertificateRequest " + key + " Ready"
	}
	detail := fmt.Sprintf("CertificateRequest %s (Ready=%s reason=%s)", key, status, reason)
	if phase1Pending {
		return CatPending, detail + " — waiting on OpenBao bootstrap"
	}
	if r, ok := MatchExternalDep(key, extDep); ok {
		return CatDeferred, detail + " — " + r
	}
	if reason == "Pending" {
		return CatOK, "CertificateRequest " + key + " pending issuer signature"
	}
	return CatFail, fmt.Sprintf("CertificateRequest %s (Ready=%s reason=%s message=%s)", key, status, reason, msg)
}

// Phase1PendingIssuers are the ClusterIssuer names expected NotReady until OpenBao
// is bootstrapped (the platform CA chain).
func Phase1PendingIssuers() []string { return []string{"platform-app-ca"} }

// Phase1PendingCerts are the Certificate namespace/names expected NotReady until
// OpenBao is bootstrapped.
func Phase1PendingCerts() []string { return []string{"openbao/openbao-tls"} }

// ExternalDepIssuers / ExternalDepCerts are operator-deferred allowlists for
// cert-manager resources (currently empty — apl-core owns its own Certs; only
// repo-shipped ones carry forward here).
func ExternalDepIssuers() []DepEntry { return nil }
func ExternalDepCerts() []DepEntry   { return nil }
