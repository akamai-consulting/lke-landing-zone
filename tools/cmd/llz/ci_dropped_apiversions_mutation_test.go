package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunCIDroppedAPIVersionsFailsOnAHit is the CI face's exit contract. This is
// the ONLY gate that reads the shared platform-apl/ tree (k8s-lint, k8s-validate
// and the kind dry-run all scan $RENDER_DIR, built from kubernetes-charts/*/
// only), and every instance fetches that tree remotely — so a verb that scanned
// the hit and then returned 0 anyway would let one stale apiVersion reach every
// instance at once, surfacing as an opaque Argo SyncFailed at deploy time.
func TestRunCIDroppedAPIVersionsFailsOnAHit(t *testing.T) {
	if len(droppedAPIs) == 0 {
		t.Skip("no dropped apiVersions currently declared")
	}
	dropped := droppedAPIs[0].apiVersion

	root := t.TempDir()
	dir := filepath.Join(root, "platform-apl", "components", "widget")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "es.yaml"),
		[]byte("apiVersion: "+dropped+"\nkind: ExternalSecret\nmetadata:\n  name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runCIDroppedAPIVersions(root)
	if err == nil {
		t.Fatalf("a manifest declaring %s must FAIL the gate — passing it ships an un-appliable manifest to every instance", dropped)
	}
	if !strings.Contains(err.Error(), "no longer serves") {
		t.Errorf("error should explain the apiVersion is no longer served, got: %v", err)
	}

	// And a clean tree of the same shape still passes, or the gate is just noise.
	if err := os.WriteFile(filepath.Join(dir, "es.yaml"),
		[]byte("apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata:\n  name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCIDroppedAPIVersions(root); err != nil {
		t.Errorf("a clean tree must pass, got: %v", err)
	}
}
