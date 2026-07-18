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
  annotations:
    argocd.argoproj.io/sync-wave: "-5"
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
	if !strings.Contains(out.String(), "1 rendered ArgoCD Application(s)/AppProject(s) passed") {
		t.Errorf("unexpected output: %q", out.String())
	}

	// A duplicate makes it fail.
	dup := `kind: Application
metadata:
  name: bad
  annotations:
    argocd.argoproj.io/sync-wave: "0"
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

func TestMissingSyncWave(t *testing.T) {
	app := func(annotations map[string]string) renderedArgoApp {
		var a renderedArgoApp
		a.Kind = "Application"
		a.Metadata.Name = "x"
		a.Metadata.Annotations = annotations
		return a
	}
	tests := []struct {
		name    string
		in      renderedArgoApp
		wantMsg string
	}{
		{"valid negative wave", app(map[string]string{"argocd.argoproj.io/sync-wave": "-5"}), ""},
		{"valid zero", app(map[string]string{"argocd.argoproj.io/sync-wave": "0"}), ""},
		{"whitespace tolerated", app(map[string]string{"argocd.argoproj.io/sync-wave": " 3 "}), ""},
		{"no annotations at all", app(nil), "has no"},
		{"other annotations only", app(map[string]string{"foo": "bar"}), "has no"},
		{
			// The shell version grepped for the annotation NAME anywhere in the file
			// and never parsed the value, so a typo'd wave passed. Argo ignores what
			// it cannot parse, which silently means wave 0.
			name: "non-integer value", in: app(map[string]string{"argocd.argoproj.io/sync-wave": "first"}),
			wantMsg: "non-integer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := missingSyncWave(tt.in)
			if tt.wantMsg == "" && got != "" {
				t.Errorf("missingSyncWave = %q, want no complaint", got)
			}
			if tt.wantMsg != "" && !strings.Contains(got, tt.wantMsg) {
				t.Errorf("missingSyncWave = %q, want it to mention %q", got, tt.wantMsg)
			}
		})
	}
}

// TestSyncWaveIsCheckedPerDocument is the regression the fold fixes. The former
// sync-wave-lint grepped whole FILES: one annotated Application satisfied the
// check for every other Application rendered beside it. Helm routinely emits
// several Applications into one file, so an un-waved one could ride along
// invisibly.
func TestSyncWaveIsCheckedPerDocument(t *testing.T) {
	dir := t.TempDir()
	multiDoc := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: waved
  annotations:
    argocd.argoproj.io/sync-wave: "-10"
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: unwaved
`
	if err := os.WriteFile(filepath.Join(dir, "apps.yaml"), []byte(multiDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runArgoCDRenderedApps(dir, &out)
	if err == nil {
		t.Fatal("an un-waved Application sharing a file with a waved one must FAIL — that is exactly what the file-scoped grep missed")
	}
}
