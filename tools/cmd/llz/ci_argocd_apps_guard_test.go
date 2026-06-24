package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustApp(t *testing.T, doc string) renderedArgoApp {
	t.Helper()
	var a renderedArgoApp
	if err := yaml.Unmarshal([]byte(doc), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return a
}

func TestDuplicateHelmParams_Source(t *testing.T) {
	app := mustApp(t, `kind: Application
metadata:
  name: demo
spec:
  source:
    helm:
      parameters:
        - name: image.tag
          value: a
        - name: replicas
          value: "2"
        - name: image.tag
          value: b
`)
	dups := duplicateHelmParams(app)
	if len(dups) != 1 || dups[0] != "image.tag" {
		t.Errorf("dups = %v, want [image.tag]", dups)
	}
}

func TestDuplicateHelmParams_None(t *testing.T) {
	app := mustApp(t, `kind: Application
metadata:
  name: demo
spec:
  source:
    helm:
      parameters:
        - name: a
        - name: b
`)
	if dups := duplicateHelmParams(app); len(dups) != 0 {
		t.Errorf("expected no dups, got %v", dups)
	}
}

func TestDuplicateHelmParams_MultiSource(t *testing.T) {
	// A duplicate spanning two entries of spec.sources[].
	app := mustApp(t, `kind: Application
metadata:
  name: demo
spec:
  sources:
    - helm:
        parameters:
          - name: shared
    - helm:
        parameters:
          - name: shared
          - name: other
`)
	dups := duplicateHelmParams(app)
	if len(dups) != 1 || dups[0] != "shared" {
		t.Errorf("dups = %v, want [shared]", dups)
	}
}

func TestDuplicateHelmParams_NoHelm(t *testing.T) {
	app := mustApp(t, `kind: Application
metadata:
  name: demo
spec:
  source:
    chart: foo
`)
	if dups := duplicateHelmParams(app); len(dups) != 0 {
		t.Errorf("expected no dups for helm-less source, got %v", dups)
	}
}

func TestRunArgoCDRenderedApps(t *testing.T) {
	dir := t.TempDir()
	clean := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: clean
spec:
  source:
    helm:
      parameters:
        - name: a
        - name: b
---
kind: ConfigMap
metadata:
  name: ignored
`
	if err := os.WriteFile(filepath.Join(dir, "apps.yaml"), []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runArgoCDRenderedApps(dir, &out); err != nil {
		t.Errorf("expected pass, got %v", err)
	}
	if !strings.Contains(out.String(), "1 rendered ArgoCD Application(s) passed") {
		t.Errorf("unexpected output: %q", out.String())
	}

	// A duplicate makes it fail.
	dup := `kind: Application
metadata:
  name: bad
spec:
  source:
    helm:
      parameters:
        - name: x
        - name: x
`
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(dup), 0o644)
	out.Reset()
	if err := runArgoCDRenderedApps(dir, &out); err == nil {
		t.Error("expected duplicate-parameter failure")
	}

	// An empty render dir is an error (the gate guards against a skipped render).
	if err := runArgoCDRenderedApps(filepath.Join(dir, "empty"), &out); err == nil {
		t.Error("expected error for missing render dir")
	}
}
