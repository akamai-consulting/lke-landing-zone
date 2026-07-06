package main

import "testing"

// auditNegativeWave flags only negative-wave kinds that are neither allowlisted, a
// name exception, nor in an excluded group — the exact set the VAP would deny.
func TestAuditNegativeWave(t *testing.T) {
	res := []liveResource{
		// vetted: allowlisted kind at a negative wave → NOT flagged.
		{group: "networking.k8s.io", kind: "NetworkPolicy", namespace: "harbor", name: "default-deny", wave: -10},
		// vetted: name exception → NOT flagged.
		{group: "cert-manager.io", kind: "Certificate", namespace: "cert-manager", name: "openbao-ca", wave: -16},
		// excluded group (argoproj.io child-App CRs) → NOT flagged.
		{group: "argoproj.io", kind: "Sensor", namespace: "llz-cert-automation", name: "haproxy-rebuild-trigger", wave: -14},
		{group: "argoproj.io", kind: "EventBus", namespace: "llz-cert-automation", name: "default", wave: -14},
		// non-negative wave → NOT flagged (only negative waves gate).
		{group: "apps", kind: "Deployment", namespace: "x", name: "ok", wave: 5},
		// UNVETTED health-checked kind at a negative wave → FLAGGED.
		{group: "apps", kind: "Deployment", namespace: "x", name: "bad", wave: -5},
		// UNVETTED, non-excluded CR at a negative wave → FLAGGED.
		{group: "monitoring.coreos.com", kind: "Probe", namespace: "monitoring", name: "sneaky", wave: -3},
	}
	got := auditNegativeWave(res)
	if len(got) != 2 {
		t.Fatalf("want 2 flagged, got %d: %+v", len(got), got)
	}
	// Sorted by group/Kind: apps/Deployment before monitoring.coreos.com/Probe.
	if got[0].name != "bad" || got[0].groupKind() != "apps/Deployment" {
		t.Errorf("first flag should be the unvetted Deployment, got %+v", got[0])
	}
	if got[1].name != "sneaky" || got[1].groupKind() != "monitoring.coreos.com/Probe" {
		t.Errorf("second flag should be the unvetted Probe, got %+v", got[1])
	}
}

// A cluster with only vetted/excluded negative-wave kinds audits clean — the state a
// converged cluster (the live census after the argoproj.io VAP fix) is in.
func TestAuditNegativeWaveClean(t *testing.T) {
	res := []liveResource{
		{group: "", kind: "Namespace", name: "monitoring", wave: -20},
		{group: "argoproj.io", kind: "Application", namespace: "argocd", name: "llz-cluster-foundation", wave: -20},
		{group: "kyverno.io", kind: "ClusterPolicy", name: "verify-llz-image-signature", wave: -15},
		{group: "cert-manager.io", kind: "ClusterIssuer", name: "llz-letsencrypt-production", wave: -5},
	}
	if got := auditNegativeWave(res); len(got) != 0 {
		t.Errorf("a vetted/excluded-only cluster must audit clean, got %+v", got)
	}
}

// parseNegativeWaveItems extracts only negative-wave items and derives group from apiVersion.
func TestParseNegativeWaveItems(t *testing.T) {
	raw := `{"items":[
	  {"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"a","namespace":"x","annotations":{"argocd.argoproj.io/sync-wave":"-5"}}},
	  {"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b","namespace":"y","annotations":{"argocd.argoproj.io/sync-wave":"3"}}},
	  {"apiVersion":"v1","kind":"Namespace","metadata":{"name":"c","annotations":{"argocd.argoproj.io/sync-wave":"-20"}}},
	  {"apiVersion":"v1","kind":"Secret","metadata":{"name":"d","namespace":"z"}}
	]}`
	got := parseNegativeWaveItems(raw)
	if len(got) != 2 {
		t.Fatalf("want 2 negative-wave items (Deployment, Namespace), got %d: %+v", len(got), got)
	}
	byName := map[string]liveResource{}
	for _, r := range got {
		byName[r.name] = r
	}
	if d := byName["a"]; d.group != "apps" || d.kind != "Deployment" || d.wave != -5 {
		t.Errorf("Deployment parsed wrong: %+v", d)
	}
	if n := byName["c"]; n.group != "" || n.kind != "Namespace" || n.wave != -20 {
		t.Errorf("core Namespace should have empty group: %+v", n)
	}
}

// Malformed JSON yields no items (never panics) — a `kubectl get` that returned an
// error string instead of a list must not crash the audit.
func TestParseNegativeWaveItemsMalformed(t *testing.T) {
	if got := parseNegativeWaveItems("error: the server doesn't have a resource type"); got != nil {
		t.Errorf("malformed payload should yield nil, got %+v", got)
	}
}
