package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The values fragment carrying every override the allowlist depends on —
// mirrors the real apl-values/values.yaml keys.
const waveGuardValuesAllOverrides = `
          resource.customizations.health.networking.k8s.io_NetworkPolicy: |
            local hs = {}
          resource.customizations.health.cert-manager.io_ClusterIssuer: |
            local hs = {}
          resource.customizations.health.external-secrets.io_ClusterSecretStore: |
            local hs = {}
          resource.customizations.health.external-secrets.io_ExternalSecret: |
            local hs = {}
          resource.customizations.health.external-secrets.io_PushSecret: |
            local hs = {}
`

func writeWaveGuardFixture(t *testing.T, rel, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// An override-backed kind at a negative wave passes when the values carry the
// override, and fails when they don't — pinning the two mechanisms together
// (deleting the values override must re-fail the guard).
func TestWaveGuardOverrideBackedKind(t *testing.T) {
	dir := writeWaveGuardFixture(t, "np.yaml", `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nudger-egress
  annotations:
    argocd.argoproj.io/sync-wave: "-18"
`)
	got, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].allowed {
		t.Fatalf("NetworkPolicy with override present: want 1 allowed finding, got %+v", got)
	}
	got, err = collectWaveHealthFindings([]string{dir}, "# no overrides")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].allowed {
		t.Fatalf("NetworkPolicy with override MISSING must fail the guard, got %+v", got)
	}
}

// An unvetted kind at a negative wave fails; the same kind at wave >= 0 (or
// with no wave annotation, defaulting to 0) is not the guard's concern.
func TestWaveGuardUnvettedKind(t *testing.T) {
	negative := `
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: early-cert
  annotations:
    argocd.argoproj.io/sync-wave: "-5"
`
	dir := writeWaveGuardFixture(t, "cert.yaml", negative)
	got, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].allowed {
		t.Fatalf("Certificate at wave -5 must be flagged, got %+v", got)
	}

	for name, doc := range map[string]string{
		"wave zero": `
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ok-cert
  annotations:
    argocd.argoproj.io/sync-wave: "0"
`,
		"no wave annotation": `
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ok-cert
`,
	} {
		dir := writeWaveGuardFixture(t, "cert.yaml", doc)
		got, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("%s: want no findings, got %+v", name, got)
		}
	}
}

// An Argo hook at a negative wave is not a tracked-tree resource, so it cannot
// health-wedge the bootstrap — the guard must not flag it, even for an otherwise
// unvetted health-checked kind (cluster-foundation's coredns-restart PostSync Job).
func TestWaveGuardSkipsHooks(t *testing.T) {
	hookJob := `
apiVersion: batch/v1
kind: Job
metadata:
  name: coredns-restart-on-custom-cm
  annotations:
    argocd.argoproj.io/sync-wave: "-9"
    argocd.argoproj.io/hook: PostSync
`
	dir := writeWaveGuardFixture(t, "job.yaml", hookJob)
	got, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a PostSync-hook Job must not be flagged, got %+v", got)
	}

	// The SAME Job without the hook annotation IS flagged — proving the skip is
	// hook-scoped, not a blanket batch/Job allow.
	plainJob := `
apiVersion: batch/v1
kind: Job
metadata:
  name: coredns-restart-on-custom-cm
  annotations:
    argocd.argoproj.io/sync-wave: "-9"
`
	dir = writeWaveGuardFixture(t, "job.yaml", plainJob)
	got, err = collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].allowed {
		t.Fatalf("a non-hook batch/Job at wave -9 must be flagged, got %+v", got)
	}
}

// Multi-doc files classify every doc; health-inert kinds pass on reason alone.
func TestWaveGuardMultiDocAndInertKinds(t *testing.T) {
	dir := writeWaveGuardFixture(t, "rbac.yaml", `
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sa
  annotations:
    argocd.argoproj.io/sync-wave: "-18"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: r
  annotations:
    argocd.argoproj.io/sync-wave: "-18"
---
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: cp
  annotations:
    argocd.argoproj.io/sync-wave: "-15"
`)
	got, err := collectWaveHealthFindings([]string{dir}, "# no overrides needed")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 findings, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if !f.allowed {
			t.Errorf("%s should be allowed without overrides: %+v", f.groupKind, f)
		}
	}
}

// The guard passes against the REAL shipped tree — this is the actual gate:
// if a future change puts an unvetted health-checked kind at a negative wave
// (or drops a load-bearing values override), this test fails at PR time
// instead of wedging a fresh-cluster bootstrap 40 minutes into an e2e run.
func TestWaveGuardRealTree(t *testing.T) {
	if err := runCIWaveHealthGuard("../../.."); err != nil {
		t.Fatalf("wave-health-guard fails on the shipped tree: %v", err)
	}
}
