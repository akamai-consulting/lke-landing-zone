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
	return []string{filepath.Join(dir, "platform-apl", "manifest"), comp}
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

// CROSS-APPLICATION wedge (blast-radius decomposition): a workload in the
// externalSecrets carved App (App-wave -10) whose Secret is produced by an
// ExternalSecret in the harbor carved App (App-wave 5). The workload's RESOURCE
// wave (10) is comfortably higher than the ES's (5), so the OLD App-blind guard —
// which compared only resource waves — waved it through. But the two live in
// DIFFERENT Apps: the harbor App is created (wave 5) long AFTER the externalSecrets
// App (wave -10), so the ES may not exist when the workload's App starts. The
// cross-App guard compares App-level waves and flags it.
func TestWaveDependencyCrossAppInversionDetected(t *testing.T) {
	xWorkload := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: needs-harbor-secret
  namespace: external-secrets
  annotations:
    argocd.argoproj.io/sync-wave: "10"
spec:
  template:
    spec:
      containers:
        - name: c
          env:
            - name: TOK
              valueFrom:
                secretKeyRef:
                  name: harbor-robot-token
                  key: token
`
	xES := `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: harbor-robot-token
  namespace: external-secrets
  annotations:
    argocd.argoproj.io/sync-wave: "5"
spec:
  target:
    name: harbor-robot-token
`
	dirs := wdWrite(t, map[string]string{
		"externalSecrets/needs-secret.yaml": xWorkload, // carved App wave -10
		"harbor/es.yaml":                    xES,       // carved App wave 5
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 {
		t.Fatalf("want 1 cross-App inversion, got %d: %+v", len(inv), inv)
	}
	if inv[0].workloadApp != "llz-externalsecrets" || inv[0].esApp != "llz-harbor" {
		t.Errorf("cross-App inversion should name both Apps, got %+v", inv[0])
	}
	// The reported waves are the APP-level waves (-10 vs 5), not the resource waves.
	if inv[0].workloadWave != -10 || inv[0].esWave != 5 {
		t.Errorf("cross-App inversion should report App-level waves (-10, 5), got %+v", inv[0])
	}
}

// The safe cross-App direction: a workload in a LATER-created App (harbor, wave 5)
// consuming an ExternalSecret in an EARLIER-created App (externalSecrets, wave -10)
// is fine — the ES App has a head start.
func TestWaveDependencyCrossAppOrderedOK(t *testing.T) {
	workload := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: harbor-consumer
  namespace: harbor
  annotations:
    argocd.argoproj.io/sync-wave: "5"
spec:
  template:
    spec:
      containers:
        - name: c
          env:
            - name: TOK
              valueFrom:
                secretKeyRef:
                  name: shared-token
                  key: token
`
	es := `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: shared-token
  namespace: harbor
  annotations:
    argocd.argoproj.io/sync-wave: "-10"
spec:
  target:
    name: shared-token
`
	dirs := wdWrite(t, map[string]string{
		"harbor/consumer.yaml":       workload, // carved App wave 5
		"externalSecrets/store.yaml": es,       // carved App wave -10
	})
	inv, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 0 {
		t.Errorf("consumer in a later App than its ES App must be clean, got: %+v", inv)
	}
}

func wdFmt(tmpl, annotations, optional string) string {
	// tmpl has two %s: the metadata block (annotations) and the trailing optional.
	return fmt.Sprintf(tmpl, annotations, optional)
}
