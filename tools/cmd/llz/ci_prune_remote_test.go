package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

func TestPruneRemoteAplValues(t *testing.T) {
	apl := filepath.Join(t.TempDir(), "apl-values")
	// A remotely-fetched component, the pinned-local one, the shared manifest base,
	// and the operator-owned trees that must survive.
	seed := []string{
		"components/openbao/x.yaml",
		"components/clusterHealthWorkflow/x.yaml",
		"_shared/manifest/kustomization.yaml",
		"_shared/values.yaml",
		"_shared/custom/x.yaml",
	}
	for _, p := range seed {
		full := filepath.Join(apl, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out strings.Builder
	if err := runPruneRemoteAplValues(apl, &out); err != nil {
		t.Fatal(err)
	}

	gone := []string{"_shared/manifest", "components/openbao"}
	kept := []string{"components/clusterHealthWorkflow", "_shared/values.yaml", "_shared/custom"}
	for _, p := range gone {
		if _, err := os.Stat(filepath.Join(apl, p)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned (fetched remotely)", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(filepath.Join(apl, p)); err != nil {
			t.Errorf("%s must survive the prune: %v", p, err)
		}
	}
	// The prune list is exactly clusterspec's remote set + _shared/manifest — the
	// pinned-local components are never in it.
	for _, name := range clusterspec.RemotelyFetchedComponents() {
		if name == "clusterHealthWorkflow" {
			t.Error("clusterHealthWorkflow must NOT be in the remotely-fetched set (its image is a WorkflowTemplate)")
		}
	}
	// Idempotent: a second run on the slimmed tree is a clean no-op.
	var out2 strings.Builder
	if err := runPruneRemoteAplValues(apl, &out2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "nothing to prune") {
		t.Errorf("second prune should be a no-op, got:\n%s", out2.String())
	}
}
