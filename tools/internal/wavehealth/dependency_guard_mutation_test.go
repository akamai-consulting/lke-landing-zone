package wavehealth

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// TestWaveDependencyKeysOnTheTargetSecretName is the rename trap. An
// ExternalSecret's spec.target.name is the Secret it actually CREATES, and it is
// routinely different from the ExternalSecret's own metadata.name. A workload
// mounts the Secret, so indexing by metadata.name silently makes every such
// ExternalSecret invisible to this guard — the dependency is not "clean", it is
// unexamined, and the #142-class bootstrap wedge walks straight through.
func TestWaveDependencyKeysOnTheTargetSecretName(t *testing.T) {
	// ExternalSecret named `linode-token-es`, creating Secret `linode-api-token`.
	es := `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: linode-token-es
  namespace: llz-reconciler
  annotations:
    argocd.argoproj.io/sync-wave: "5"
spec:
  target:
    name: linode-api-token
`
	dirs := wdWrite(t, map[string]string{
		"llzReconciler/deployment.yaml":     wdFmt(wdReconcilerDeploy, "", ""), // wave 0, refs linode-api-token
		"llzReconciler/externalsecret.yaml": es,
	})
	inv, _, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 {
		t.Fatalf("want 1 inversion keyed on spec.target.name, got %d: %+v — an ExternalSecret whose target Secret is named differently from itself must still be matched", len(inv), inv)
	}
	if inv[0].secret != "linode-api-token" || inv[0].esWave != 5 {
		t.Errorf("unexpected inversion: %+v", inv[0])
	}
}

// TestWaveDependencyEqualAppWavesRace is the cross-App boundary. Two carved
// Applications created in the SAME App-level wave have no ordering guarantee
// between them and no cross-App health gate, so a workload in one consuming an
// ExternalSecret in the other is a race — not a pass. Treating equal waves as
// safe is exactly the wedge risk the App-level comparison was added for.
func TestWaveDependencyEqualAppWavesRace(t *testing.T) {
	// harbor and llzReconciler are both carved at App wave 5.
	harbor, ok := clusterspec.LookupComponent("harbor")
	if !ok || harbor.CarvedApp == nil {
		t.Skip("harbor is no longer a carved component")
	}
	rec, ok := clusterspec.LookupComponent("llzReconciler")
	if !ok || rec.CarvedApp == nil {
		t.Skip("llzReconciler is no longer a carved component")
	}
	if harbor.CarvedApp.AppWave != rec.CarvedApp.AppWave {
		t.Skipf("harbor (%d) and llzReconciler (%d) no longer share an App wave",
			harbor.CarvedApp.AppWave, rec.CarvedApp.AppWave)
	}

	workload := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: harbor-consumer
  namespace: harbor
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
spec:
  target:
    name: shared-token
`
	dirs := wdWrite(t, map[string]string{
		"harbor/consumer.yaml":     workload, // carved App llz-harbor, wave 5
		"llzReconciler/store.yaml": es,       // carved App llz-reconciler, wave 5
	})
	inv, _, err := collectWaveDependencyInversions(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 {
		t.Fatalf("two carved Apps in the SAME wave race — that must be flagged, got %d: %+v", len(inv), inv)
	}
	if inv[0].workloadApp == inv[0].esApp {
		t.Errorf("the finding should name two DIFFERENT Apps, got %+v", inv[0])
	}
}

// TestWDComponentOfResolvesAtEveryOffset: the component a manifest belongs to is
// found by locating the "/components/" segment in its path. The path comes from
// the walk, so its prefix depends entirely on where the scan was rooted — a
// resolver that only works when something precedes the marker silently demotes
// every manifest to the platform-bootstrap root App, and every cross-App
// comparison it feeds becomes wrong.
func TestWDComponentOfResolvesAtEveryOffset(t *testing.T) {
	for _, path := range []string{
		"platform-apl/components/harbor/es.yaml",
		"/tmp/x/components/harbor/es.yaml",
		"/components/harbor/es.yaml", // scan rooted at the filesystem root
	} {
		c, ok := wdComponentOf(path)
		if !ok || c.Name != "harbor" {
			t.Errorf("wdComponentOf(%q) = (%q,%v), want harbor,true", path, c.Name, ok)
		}
	}
	// A path outside any component dir belongs to the root tree.
	if _, ok := wdComponentOf("platform-apl/manifest/base.yaml"); ok {
		t.Error("a non-component path must not resolve to a component")
	}
}
