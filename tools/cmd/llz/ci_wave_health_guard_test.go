package main

import (
	"os"
	"path/filepath"
	"strings"
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
	got, _, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].allowed {
		t.Fatalf("NetworkPolicy with override present: want 1 allowed finding, got %+v", got)
	}
	got, _, err = collectWaveHealthFindings([]string{dir}, "# no overrides")
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
	got, _, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
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
		got, _, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
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
	got, _, err := collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
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
	got, _, err = collectWaveHealthFindings([]string{dir}, waveGuardValuesAllOverrides)
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
	got, _, err := collectWaveHealthFindings([]string{dir}, "# no overrides needed")
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

// An empty corpus must FAIL, not pass. The guard used to call walkManifests and
// throw the examined count away, so with its trees absent or moved it walked zero
// files, classified zero negative-wave kinds, and printed the same "every
// negative-wave kind is health-safe" green as a full clean run — the PR #142 wedge
// class unpoliced by a guard that reported success. Its three siblings all gated on
// requireCorpus; this one did not.
func TestWaveHealthGuardFailsOnEmptyCorpus(t *testing.T) {
	// A root with an apl-values/values.yaml (so the guard gets past the values
	// read) but no platform-apl tree at all: nothing to examine.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "apl-values"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "apl-values", "values.yaml"),
		[]byte(waveGuardValuesAllOverrides), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, examined, err := collectWaveHealthFindings(platformTreeDirs(root), waveGuardValuesAllOverrides)
	if err != nil {
		t.Fatalf("an absent tree is skipped, not a walk error: %v", err)
	}
	if len(findings) != 0 || examined != 0 {
		t.Fatalf("empty corpus: want 0 findings / 0 examined, got %d / %d", len(findings), examined)
	}

	err = runCIWaveHealthGuard(root)
	if err == nil {
		t.Fatal("wave-health-guard passed with an EMPTY corpus — a guard that examined nothing must not report the same green as one that examined everything")
	}
	if !strings.Contains(err.Error(), "examined 0 manifest files") {
		t.Fatalf("want the requireCorpus failure, got: %v", err)
	}

	// Control: the same root WITH one manifest gets past the corpus gate (it fails
	// on the unvetted kind instead), so the gate keys off the corpus being empty
	// and not off some other property of the temp layout.
	p := filepath.Join(root, "platform-apl", "manifest", "cert.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("kind: ConfigMap\nmetadata:\n  name: c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCIWaveHealthGuard(root); err != nil {
		t.Fatalf("one health-inert manifest is a non-empty corpus and must pass: %v", err)
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
