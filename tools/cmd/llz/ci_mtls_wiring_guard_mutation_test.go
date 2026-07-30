package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mtlsTree writes files under <root>/platform-apl/components/<rel> and returns
// the root, matching the layout platformTreeDirs scans.
func mtlsTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, "platform-apl", "components", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const mtlsUnwiredDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: llz-reconciler
  namespace: llz-reconciler
spec:
  template:
    spec:
      containers:
        - name: reconcile
          env:
            - {name: OPENBAO_ADDR, value: "https://openbao:8200"}
`

// TestMTLSWiringGuardVerbFailsOnAFinding is the end-to-end exit contract for
// `llz ci mtls-wiring-guard`. The unit-level checkMTLSWiring tests prove the
// invariant; this proves the VERB carries a finding out as a non-zero exit.
// Every step between — the doc filter that decides which YAML documents are
// workloads at all, and the two error checks around the walk — can drop a real
// finding on the floor while the guard still prints its cheerful summary line,
// which is precisely the silent-green this family of guards exists to prevent.
func TestMTLSWiringGuardVerbFailsOnAFinding(t *testing.T) {
	root := mtlsTree(t, map[string]string{
		"llzReconciler/deployment.yaml": mtlsUnwiredDeployment,
	})

	var err error
	out := captureStdout(t, func() { err = runCIMTLSWiringGuard(root) })
	if err == nil {
		t.Fatalf("an OpenBao consumer mounting no TLS material must FAIL the guard; stdout:\n%s", out)
	}
	if !strings.Contains(err.Error(), "problem(s)") {
		t.Errorf("error should count the problems, got: %v", err)
	}
	if !strings.Contains(out, "::error file=") {
		t.Errorf("each finding must be emitted as a PR annotation:\n%s", out)
	}
	if strings.Contains(out, "every OpenBao consumer mounts") {
		t.Errorf("the guard must not print its all-clear line alongside a finding:\n%s", out)
	}
}

// TestMTLSWiringGuardVerbPassesAWiredTree is the other half — the same walk over
// a correctly wired workload must be clean, so the failure above is the finding
// and not the harness.
func TestMTLSWiringGuardVerbPassesAWiredTree(t *testing.T) {
	certs := `apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: openbao-ca-bundle, namespace: llz-reconciler}
spec: {secretName: openbao-ca-bundle}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: llz-reconciler-client-tls, namespace: llz-reconciler}
spec: {secretName: llz-reconciler-client-tls}
`
	root := mtlsTree(t, map[string]string{
		"llzReconciler/deployment.yaml": "apiVersion: apps/v1" + wiredDeployment,
		"llzReconciler/certs.yaml":      certs,
	})
	if err := runCIMTLSWiringGuard(root); err != nil {
		t.Fatalf("a correctly wired tree must pass: %v", err)
	}
}

// TestMTLSWiringGuardRefusesAnEmptyCorpus: a guard that walked nothing reports
// the same green as one that walked everything. This is the sibling guards'
// shared contract, and the only thing standing between a moved manifest tree and
// a permanently inert gate.
func TestMTLSWiringGuardRefusesAnEmptyCorpus(t *testing.T) {
	root := t.TempDir() // no platform-apl/ at all
	if err := runCIMTLSWiringGuard(root); err == nil {
		t.Fatal("an empty corpus must fail — 'walked nothing' must not read as 'all clean'")
	}
}
