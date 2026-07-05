package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// wdWrite drops a manifest into a temp component dir and returns the dir list
// collectWaveDependencyInversions expects.
func wdWrite(t *testing.T, files map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	comp := filepath.Join(dir, "components")
	for name, body := range files {
		p := filepath.Join(comp, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return []string{filepath.Join(dir, "_shared", "manifest"), comp}
}

const wdReconcilerDeploy = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: llz-reconciler
  namespace: llz-reconciler
%s
spec:
  template:
    spec:
      containers:
        - name: reconcile
          env:
            - name: LINODE_TOKEN
              valueFrom:
                secretKeyRef:
                  name: linode-api-token
                  key: token%s
`

const wdReconcilerES = `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: linode-api-token
  namespace: llz-reconciler
  annotations:
    argocd.argoproj.io/sync-wave: "5"
spec:
  target:
    name: linode-api-token
`

// The #163 bug: a wave-0 Deployment (no annotation) with a hard secretKeyRef on
// a wave-5 ExternalSecret's Secret → one inversion.
func TestWaveDependencyInversionDetected(t *testing.T) {
	dirs := wdWrite(t, map[string]string{
		"llzReconciler/deployment.yaml":     wdFmt(wdReconcilerDeploy, "", ""), // no sync-wave → wave 0
		"llzReconciler/externalsecret.yaml": wdReconcilerES,
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 {
		t.Fatalf("want 1 inversion, got %d: %+v", len(inv), inv)
	}
	if inv[0].secret != "linode-api-token" || inv[0].workloadWave != 0 || inv[0].esWave != 5 {
		t.Errorf("unexpected inversion: %+v", inv[0])
	}
}

// The fix: Deployment at wave 6 (> the ES's wave 5) → no inversion.
func TestWaveDependencyOrderedOK(t *testing.T) {
	dirs := wdWrite(t, map[string]string{
		"llzReconciler/deployment.yaml": wdFmt(wdReconcilerDeploy, `  annotations:
    argocd.argoproj.io/sync-wave: "6"`, ""),
		"llzReconciler/externalsecret.yaml": wdReconcilerES,
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 0 {
		t.Errorf("wave 6 > ES wave 5 must be clean, got: %+v", inv)
	}
}

// An optional reference doesn't block pod start, so it's not a wedge even at an
// earlier wave.
func TestWaveDependencyOptionalSkipped(t *testing.T) {
	dirs := wdWrite(t, map[string]string{
		"llzReconciler/deployment.yaml":     wdFmt(wdReconcilerDeploy, "", "\n                  optional: true"),
		"llzReconciler/externalsecret.yaml": wdReconcilerES,
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 0 {
		t.Errorf("optional secretKeyRef must be ignored, got: %+v", inv)
	}
}

// Same wave (workload == ES) is allowed: both apply in the same wave, so the
// Secret can be produced; only a STRICTLY later ES wave wedges.
func TestWaveDependencySameWaveOK(t *testing.T) {
	dirs := wdWrite(t, map[string]string{
		"llzReconciler/deployment.yaml": wdFmt(wdReconcilerDeploy, `  annotations:
    argocd.argoproj.io/sync-wave: "5"`, ""),
		"llzReconciler/externalsecret.yaml": wdReconcilerES,
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 0 {
		t.Errorf("same-wave (5 == 5) must be clean, got: %+v", inv)
	}
}

// A Secret with no ExternalSecret in the tree (e.g. statically seeded / apl-core
// managed) is not this guard's concern → no false positive.
func TestWaveDependencyNoMatchingES(t *testing.T) {
	dirs := wdWrite(t, map[string]string{
		"llzReconciler/deployment.yaml": wdFmt(wdReconcilerDeploy, "", ""),
		// no ExternalSecret for linode-api-token
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 0 {
		t.Errorf("a Secret with no ExternalSecret in-tree must not flag, got: %+v", inv)
	}
}

// A cross-namespace name collision must NOT match: the Secret is namespace
// scoped, so a same-named ExternalSecret in a different namespace is unrelated.
func TestWaveDependencyNamespaceScoped(t *testing.T) {
	otherNsES := `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: linode-api-token
  namespace: some-other-ns
  annotations:
    argocd.argoproj.io/sync-wave: "5"
spec:
  target:
    name: linode-api-token
`
	dirs := wdWrite(t, map[string]string{
		"llzReconciler/deployment.yaml": wdFmt(wdReconcilerDeploy, "", ""),
		"other/externalsecret.yaml":     otherNsES,
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 0 {
		t.Errorf("a same-named ES in a different namespace must not match, got: %+v", inv)
	}
}

func wdFmt(tmpl, annotations, optional string) string {
	// tmpl has two %s: the metadata block (annotations) and the trailing optional.
	return fmt.Sprintf(tmpl, annotations, optional)
}
