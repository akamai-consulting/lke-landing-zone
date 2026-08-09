package manifestguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The passed-app tally is asserted EXACTLY, not by substring. A substring check
// for "2 rendered" also matches the sign-flipped "-2 rendered" a decrementing
// counter would print, so the count was effectively unchecked.
func TestRunArgoCDRenderedAppsTallyIsExact(t *testing.T) {
	dir := t.TempDir()
	doc := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: one
  annotations:
    argocd.argoproj.io/sync-wave: "-5"
---
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: two
  annotations:
    argocd.argoproj.io/sync-wave: "0"
`
	if err := os.WriteFile(filepath.Join(dir, "apps.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := RunArgoCDRenderedApps(dir, &out); err != nil {
		t.Fatalf("both documents are well-formed: %v", err)
	}
	const want = "2 rendered ArgoCD Application(s)/AppProject(s) passed semantic validation.\n"
	if out.String() != want {
		t.Errorf("summary = %q, want exactly %q", out.String(), want)
	}
}

// The ::error annotation must name the offending Application. "<unknown>" is
// reserved for a document with no metadata.name at all — an operator who gets it
// for a named app cannot find the app.
func TestRunArgoCDRenderedAppsAnnotationNamesTheApp(t *testing.T) {
	t.Run("named app is named", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "apps.yaml"), []byte(
			"apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: unwaved-app\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		var err error
		errOut := captureStderr(t, func() { err = RunArgoCDRenderedApps(dir, &out) })
		if err == nil {
			t.Fatal("an un-waved Application must fail the guard")
		}
		if !strings.Contains(errOut, `"unwaved-app"`) {
			t.Errorf("annotation must name the app, got:\n%s", errOut)
		}
		if strings.Contains(errOut, "<unknown>") {
			t.Errorf("a named app must not be reported as <unknown>:\n%s", errOut)
		}
	})

	t.Run("nameless app falls back to <unknown>", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "apps.yaml"), []byte(
			"apiVersion: argoproj.io/v1alpha1\nkind: Application\nspec: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		var err error
		errOut := captureStderr(t, func() { err = RunArgoCDRenderedApps(dir, &out) })
		if err == nil {
			t.Fatal("an un-waved Application must fail the guard")
		}
		if !strings.Contains(errOut, "<unknown>") {
			t.Errorf("a nameless app must be reported as <unknown>, got:\n%s", errOut)
		}
	})
}
