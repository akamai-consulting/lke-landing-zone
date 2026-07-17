package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// writeCustom builds a kubernetes-custom/ tree under a temp instance root and returns
// the customDir checkCustomLayout takes. files maps a path relative to that tree to its
// content; a "" content creates the directory only.
func writeCustom(t *testing.T, files map[string]string) string {
	t.Helper()
	customDir := filepath.Join(t.TempDir(), clusterspec.CustomRoot)
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

// An absent tree is the template-repo case — silent, not an error.
func TestCheckCustomLayout_AbsentIsFine(t *testing.T) {
	if err := checkCustomLayout(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("absent kubernetes-custom/ must not error: %v", err)
	}
}

// The documented layout passes, including a namespace dir carrying its own kustomize
// root (optional, but supported).
func TestCheckCustomLayout_ValidLayoutPasses(t *testing.T) {
	dir := writeCustom(t, map[string]string{
		"README.md":                         "# hatch",
		"namespaces/my-app/deployment.yaml": "kind: Deployment\n",
		"namespaces/my-app/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\n" +
			"kind: Kustomization\nresources:\n  - deployment.yaml\n",
		"namespaces/argocd/app.yaml": "kind: Application\n",
		"global/crd.yaml":            "kind: CustomResourceDefinition\n",
	})
	if err := checkCustomLayout(dir); err != nil {
		t.Errorf("valid layout must pass: %v", err)
	}
}

// A tree with no namespaces/ at all is legal — the generator simply yields zero Apps.
func TestCheckCustomLayout_NoNamespacesDirIsFine(t *testing.T) {
	dir := writeCustom(t, map[string]string{"README.md": "# hatch"})
	if err := checkCustomLayout(dir); err != nil {
		t.Errorf("a tree without namespaces/ must not error: %v", err)
	}
}

// apl-* namespaces belong to apl-core's own gitops-ns-apl-* Applications; a second
// Application over the same resources puts them in contention.
func TestCheckCustomLayout_ReservedAplPrefixBlocks(t *testing.T) {
	dir := writeCustom(t, map[string]string{"namespaces/apl-secrets/secret.yaml": "kind: Secret\n"})
	err := checkCustomLayout(dir)
	if err == nil {
		t.Fatal("namespaces/apl-* must block")
	}
	for _, want := range []string{"apl-secrets", "contention", "reserved", "gitops-ns-apl-secrets"} {
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

// A FILE named apl-something (rather than a directory) generates no Application, so it
// is not a collision and must not block.
func TestCheckCustomLayout_ReservedPrefixOnlyAppliesToDirs(t *testing.T) {
	dir := writeCustom(t, map[string]string{"namespaces/apl-notes.md": "not a namespace"})
	if err := checkCustomLayout(dir); err != nil {
		t.Errorf("a file named apl-* is not a namespace dir: %v", err)
	}
}

// Every finding surfaces at once — an operator fixing one thing per render is a bad loop.
func TestCheckCustomLayout_ReportsAllFindings(t *testing.T) {
	dir := writeCustom(t, map[string]string{
		"namespaces/apl-users/u.yaml": "kind: ConfigMap\n",
		"namespaces/global/g.yaml":    "kind: ConfigMap\n",
	})
	err := checkCustomLayout(dir)
	if err == nil {
		t.Fatal("expected findings")
	}
	for _, want := range []string{"apl-users", "collide"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected all findings at once, missing %q:\n%s", want, err)
		}
	}
}
