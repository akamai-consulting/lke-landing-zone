package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCustom builds a custom/ tree under a temp apl-values dir and returns the
// customDir checkCustomLayout takes. files maps a path relative to custom/ to its
// content; a "" content creates the directory only.
func writeCustom(t *testing.T, files map[string]string) string {
	t.Helper()
	customDir := filepath.Join(t.TempDir(), "_shared", "custom")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(customDir, rel)
		if content == "" {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return customDir
}

// An absent custom/ is the template-repo case — silent, not an error.
func TestCheckCustomLayout_AbsentIsFine(t *testing.T) {
	if err := checkCustomLayout(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("absent custom/ must not error: %v", err)
	}
}

// The new layout passes.
func TestCheckCustomLayout_NewLayoutPasses(t *testing.T) {
	dir := writeCustom(t, map[string]string{
		"README.md":                         "# hatch",
		"namespaces/my-app/deployment.yaml": "kind: Deployment\n",
		"namespaces/argocd/app.yaml":        "kind: Application\n",
		"global/crd.yaml":                   "kind: CustomResourceDefinition\n",
	})
	if err := checkCustomLayout(dir); err != nil {
		t.Errorf("new layout must pass: %v", err)
	}
}

// The untouched empty starter is inert — nothing was deployed through it, so there is
// nothing to cascade-delete and no reason to fail a fresh instance.
func TestCheckCustomLayout_EmptyFlatStarterPasses(t *testing.T) {
	dir := writeCustom(t, map[string]string{
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n",
	})
	if err := checkCustomLayout(dir); err != nil {
		t.Errorf("empty flat starter must not block: %v", err)
	}
}

// THE case this guard exists for: a flat root carrying content. Rendering over it would
// prune the old Application and cascade-delete the operator's workloads.
func TestCheckCustomLayout_FlatWithContentBlocks(t *testing.T) {
	dir := writeCustom(t, map[string]string{
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - my-app.yaml\n",
		"my-app.yaml":        "kind: Deployment\n",
	})
	err := checkCustomLayout(dir)
	if err == nil {
		t.Fatal("flat layout with content MUST block the render")
	}
	// The message has to carry the migration, not just the complaint.
	for _, want := range []string{"FLAT layout", "namespaces/", "global/", "CASCADE-DELETE", "llz render"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("migration hint missing %q:\n%s", want, err)
		}
	}
}

// Content declared through any kustomize field counts, not just `resources:` — an
// operator using helmCharts or generators has just as much to lose.
func TestCheckCustomLayout_FlatNonResourceFieldsBlock(t *testing.T) {
	for name, body := range map[string]string{
		"helmCharts":         "helmCharts:\n  - name: nginx\n    repo: https://example.com\n",
		"configMapGenerator": "configMapGenerator:\n  - name: cm\n    literals: [a=b]\n",
		"components":         "components:\n  - ../thing\n",
		"namespace":          "namespace: my-app\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeCustom(t, map[string]string{
				"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" + body,
			})
			if err := checkCustomLayout(dir); err == nil {
				t.Errorf("flat kustomization declaring %s must block", name)
			}
		})
	}
}

// An unparseable kustomization can't be proven empty, so it blocks rather than being
// waved through into a destructive render.
func TestCheckCustomLayout_UnparseableFlatBlocks(t *testing.T) {
	dir := writeCustom(t, map[string]string{"kustomization.yaml": "\t:::not yaml:::\n  - [\n"})
	if err := checkCustomLayout(dir); err == nil {
		t.Error("unparseable flat kustomization must block")
	}
}

// Manifests loose in custom/ are matched by no generator — they would silently stop
// being applied, which is worse than a failed render.
func TestCheckCustomLayout_StrayRootManifestsBlock(t *testing.T) {
	dir := writeCustom(t, map[string]string{"orphan.yaml": "kind: ConfigMap\n"})
	err := checkCustomLayout(dir)
	if err == nil {
		t.Fatal("stray root manifests must block")
	}
	if !strings.Contains(err.Error(), "orphan.yaml") {
		t.Errorf("error should name the stray file:\n%s", err)
	}
	// A README beside them is not a manifest and must not trip the check.
	ok := writeCustom(t, map[string]string{"README.md": "# hatch", "namespaces/README.md": "# ns"})
	if err := checkCustomLayout(ok); err != nil {
		t.Errorf("non-manifest files in the root must not block: %v", err)
	}
}

// apl-* namespaces belong to apl-core's own gitops-ns-apl-* Applications.
func TestCheckCustomLayout_ReservedAplPrefixBlocks(t *testing.T) {
	dir := writeCustom(t, map[string]string{"namespaces/apl-secrets/secret.yaml": "kind: Secret\n"})
	err := checkCustomLayout(dir)
	if err == nil {
		t.Fatal("namespaces/apl-* must block")
	}
	for _, want := range []string{"apl-secrets", "contention", "reserved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("apl-prefix error missing %q:\n%s", want, err)
		}
	}
	// A namespace that merely CONTAINS "apl" is not reserved — only the prefix is.
	ok := writeCustom(t, map[string]string{"namespaces/my-apl-thing/x.yaml": "kind: ConfigMap\n"})
	if err := checkCustomLayout(ok); err != nil {
		t.Errorf("non-prefix apl match must not block: %v", err)
	}
}

// namespaces/global would generate instance-custom-global, colliding with the App the
// global/ generator emits.
func TestCheckCustomLayout_ReservedGlobalNameBlocks(t *testing.T) {
	dir := writeCustom(t, map[string]string{"namespaces/global/x.yaml": "kind: ConfigMap\n"})
	err := checkCustomLayout(dir)
	if err == nil {
		t.Fatal("namespaces/global must block")
	}
	if !strings.Contains(err.Error(), "collide") {
		t.Errorf("global collision error should explain the collision:\n%s", err)
	}
}

// Every finding surfaces at once — an operator fixing one thing per render is a bad loop.
func TestCheckCustomLayout_ReportsAllFindings(t *testing.T) {
	dir := writeCustom(t, map[string]string{
		"kustomization.yaml":          "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - x.yaml\n",
		"namespaces/apl-users/u.yaml": "kind: ConfigMap\n",
		"namespaces/global/g.yaml":    "kind: ConfigMap\n",
	})
	err := checkCustomLayout(dir)
	if err == nil {
		t.Fatal("expected findings")
	}
	for _, want := range []string{"FLAT layout", "apl-users", "collide"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected all findings at once, missing %q:\n%s", want, err)
		}
	}
}
