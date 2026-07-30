package main

import (
	"os"
	"path/filepath"
	"strings"
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
	f, _, err := collectMeshEgressFindings(dirs)
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
	f, _, err := collectMeshEgressFindings(dirs)
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
	f, _, err := collectMeshEgressFindings(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 0 {
		t.Errorf("egress to a non-STRICT-mesh namespace must not flag, got: %+v", f)
	}
}

// The corpus must include the RENDERED charts. Chart-shipped NetworkPolicies are
// invisible any other way: their templates/ dirs are skipped by walkManifests and
// their target namespaces are Helm values. Pointing at kubernetes-charts/ instead
// is the plausible wrong fix, so this pins the right one.
func TestMeshEgressScanDirsIncludeRenderedCharts(t *testing.T) {
	var rendered bool
	for _, d := range meshEgressScanDirs("/repo") {
		if filepath.Base(d) == renderedChartsDir {
			rendered = true
		}
	}
	if !rendered {
		t.Fatalf("the rendered chart tree must be scanned; got %v", meshEgressScanDirs("/repo"))
	}
}

// renderedChartsDir must track the Makefile's RENDER_DIR. If they drift, the
// guard scans a directory the build never fills and silently returns to the blind
// spot this change closed.
func TestMeshEgressRenderedDirMatchesMakefile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "Makefile"))
	if err != nil {
		t.Skipf("Makefile not readable from here: %v", err)
	}
	if !strings.Contains(string(b), "RENDER_DIR ?= "+renderedChartsDir) {
		t.Fatalf("RENDER_DIR in the Makefile no longer matches renderedChartsDir=%q", renderedChartsDir)
	}
}

// A missing rendered tree is a hard error, never a quiet pass. Without this the
// guard reports the same green whether or not it saw the chart policies.
func TestMeshEgressRequiresRenderedTree(t *testing.T) {
	if err := requireRenderedCharts(t.TempDir()); err == nil {
		t.Fatal("a missing rendered tree must fail, not pass green")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, renderedChartsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requireRenderedCharts(dir); err != nil {
		t.Fatalf("a present rendered tree must pass: %v", err)
	}
}

// A rendered chart policy naming a STRICT namespace is caught, and the allowlist
// is what decides whether it fails the build.
func TestMeshEgressFlagsRenderedChartPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, renderedChartsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, renderedChartsDir, "c.yaml"),
		[]byte(meNetpol("runner-egress", "llz-cert-automation", "harbor")), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, examined, err := collectMeshEgressFindings(meshEgressScanDirs(dir))
	if err != nil {
		t.Fatal(err)
	}
	if examined == 0 {
		t.Fatal("examined 0 files — the rendered tree was not walked")
	}
	if len(fs) != 1 || fs[0].targetNS != "harbor" {
		t.Fatalf("want 1 harbor finding from the rendered tree, got %+v", fs)
	}
	// Unregistered by default...
	un, _ := filterMeshEgressAllowed(fs)
	if len(un) != 1 {
		t.Fatalf("an unregistered rule must survive the filter, got %+v", un)
	}
	// ...and registered once its key is in the allowlist.
	k := "llz-cert-automation/runner-egress->harbor"
	meshEgressAllowed[k] = meshEgressRule{reason: "test", owner: "llz"}
	defer delete(meshEgressAllowed, k)
	un2, seen := filterMeshEgressAllowed(fs)
	if len(un2) != 0 || !seen[k] {
		t.Fatalf("a registered rule must be filtered out and marked seen; got %+v seen=%v", un2, seen)
	}
}
