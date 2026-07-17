package main

import (
	"os"
	"path/filepath"
	"testing"
)

// meWrite drops NetworkPolicy manifests into a temp components dir.
func meWrite(t *testing.T, files map[string]string) []string {
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

func meNetpol(name, ns, targetNS string) string {
	return `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ` + name + `
  namespace: ` + ns + `
spec:
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ` + targetNS + `
      ports:
        - { protocol: TCP, port: 8080 }
`
}

// The harbor-reconciler regression: a NetworkPolicy in llz-reconciler egressing to
// the STRICT-mesh harbor namespace → one finding.
func TestMeshEgressFlagsCrossMeshToHarbor(t *testing.T) {
	dirs := meWrite(t, map[string]string{
		"llzReconciler/network-policy.yaml": meNetpol("llz-reconciler", "llz-reconciler", "harbor"),
	})
	f, err := collectMeshEgressFindings(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(f), f)
	}
	if f[0].sourceNS != "llz-reconciler" || f[0].targetNS != "harbor" {
		t.Errorf("unexpected finding: %+v", f[0])
	}
}

// Same-namespace egress (harbor's own robot-provisioner CronJob → harbor-core) is
// in-mesh and must NOT flag.
func TestMeshEgressAllowsSameNamespace(t *testing.T) {
	dirs := meWrite(t, map[string]string{
		"harbor/harbor-robot-provisioner/network-policy.yaml": meNetpol("harbor-robot-provisioner-egress", "harbor", "harbor"),
	})
	f, err := collectMeshEgressFindings(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 0 {
		t.Errorf("same-namespace (harbor→harbor) egress must not flag, got: %+v", f)
	}
}

// Egress to a non-mesh namespace (e.g. llz-openbao) is fine.
func TestMeshEgressAllowsNonMeshTarget(t *testing.T) {
	dirs := meWrite(t, map[string]string{
		"llzReconciler/network-policy.yaml": meNetpol("llz-reconciler", "llz-reconciler", "llz-openbao"),
	})
	f, err := collectMeshEgressFindings(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 0 {
		t.Errorf("egress to a non-STRICT-mesh namespace must not flag, got: %+v", f)
	}
}
