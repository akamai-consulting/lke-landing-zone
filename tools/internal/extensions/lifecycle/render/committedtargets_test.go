package render

// committedTargets is the render path, not the topology reader — this test lives
// beside its subject in internal/render. Another passenger on env_set_test.go.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func TestCommittedTargets(t *testing.T) {
	chdirTempDir(t)
	e := clusterspec.Environment{Components: map[string]clusterspec.ComponentToggle{}} // all default-enabled

	targets, err := committedTargets("lab", e, clusterspec.ValuesIdentity{}, "apl-values", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join("apl-values", "lab", "manifest", "kustomization.yaml"),
		filepath.Join("apl-values", "lab", "manifest", "env-revision-configmap.yaml"),
		// llzReconciler is carved + default-on: its App CR lands in manifest/, its
		// per-env patch moves into the App's own apps/<name>/ source root.
		filepath.Join("apl-values", "lab", "manifest", "llz-reconciler.yaml"),
		filepath.Join("apl-values", "lab", "apps", "llzReconciler", "kustomization.yaml"),
		filepath.Join("apl-values", "lab", "apps", "llzReconciler", "llz-reconciler-env-patch.yaml"),
	} {
		if _, ok := targets[p]; !ok {
			t.Errorf("missing committed target %s", p)
		}
	}
	// apl-core values.yaml is NOT a target on the managed platform (Linode owns it).
	if _, ok := targets[filepath.Join("apl-values", "lab", "values.yaml")]; ok {
		t.Error("managed render must NOT emit an apl-core values.yaml")
	}
	overlay := targets[filepath.Join("apl-values", "lab", "manifest", "kustomization.yaml")]
	if !strings.Contains(overlay, "../../../../platform-apl/manifest") {
		t.Errorf("overlay is not thin (no shared base ref):\n%s", overlay)
	}
	// llzReconciler disabled → no reconciler App CR or source root at all.
	off := clusterspec.Environment{Components: map[string]clusterspec.ComponentToggle{"llzReconciler": {Enabled: boolPtrLocal(false)}}}
	t2, _ := committedTargets("lab", off, clusterspec.ValuesIdentity{}, "apl-values", nil)
	for _, p := range []string{
		filepath.Join("apl-values", "lab", "manifest", "llz-reconciler.yaml"),
		filepath.Join("apl-values", "lab", "apps", "llzReconciler", "llz-reconciler-env-patch.yaml"),
	} {
		if _, ok := t2[p]; ok {
			t.Errorf("disabled llzReconciler should not emit %s", p)
		}
	}
}
