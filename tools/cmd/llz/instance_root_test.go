package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdir (checks_test.go) moves into dir for the test and restores the CWD after.

func TestIsInstanceRootMarkers(t *testing.T) {
	// Any ONE marker is enough: a fresh `llz new` has only the copier answers, a
	// pre-spec instance only the TF roots, and the template checkout neither.
	for _, marker := range instanceRootMarkers {
		dir := t.TempDir()
		p := filepath.Join(dir, marker)
		if marker == "terraform-iac-bootstrap" {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !isInstanceRoot(dir) {
			t.Errorf("%s alone should identify an instance root", marker)
		}
	}
	if dir := t.TempDir(); isInstanceRoot(dir) {
		t.Error("an empty dir must not read as an instance root")
	}
	// The template repo itself: instanceLayout resolves against instance-template/,
	// so template CI's scaffold checks must keep working.
	tmpl := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpl, "instance-template", "terraform-iac-bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isInstanceRoot(tmpl) {
		t.Error("a template checkout must count as a valid working root")
	}
}

func TestRequireInstanceRootPointsAtTheInstance(t *testing.T) {
	// The exact quickstart slip: `llz new my-instance` succeeded, `cd my-instance`
	// was skipped, and `llz env add` ran one directory up. The error must name the
	// directory to cd into rather than just refusing.
	parent := t.TempDir()
	if err := os.MkdirAll(filepath.Join(parent, "my-instance"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "my-instance", ".copier-answers.yml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, parent)

	err := requireInstanceRoot("`llz env add`")
	if err == nil {
		t.Fatal("expected a refusal outside an instance root")
	}
	for _, want := range []string{"llz env add", "cd my-instance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestRequireInstanceRootWithNoCandidateSuggestsNew(t *testing.T) {
	chdir(t, t.TempDir())
	err := requireInstanceRoot("`llz env add`")
	if err == nil {
		t.Fatal("expected a refusal outside an instance root")
	}
	if !strings.Contains(err.Error(), "llz new") {
		t.Errorf("with no instance in sight the error should point at `llz new`; got %q", err)
	}
}

func TestRunEnvAddRefusesOutsideAnInstance(t *testing.T) {
	// The whole point of the gate: no files are written. Before it, this call
	// authored a full landingzone.yaml + environments/ + apl-values/ tree here.
	dir := t.TempDir()
	chdir(t, dir)

	err := runEnvAdd(globalOpts{}, "lab", envAddOpts{region: "us-sea", objCluster: "us-sea-1"})
	if err == nil {
		t.Fatal("expected `llz env add` to refuse outside an instance root")
	}
	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Errorf("nothing must be written outside an instance root; found %v", ents)
	}
}

func TestRequireInstanceRootFromInsideAnInstance(t *testing.T) {
	// One level too deep (terraform-iac-bootstrap/, apl-values/<env>/…) is the
	// same mistake and just as silent — and telling THIS operator to run `llz new`
	// would be nonsense. Point up at the root they are already inside.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".copier-answers.yml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "terraform-iac-bootstrap", "cluster")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, deep)

	err := requireInstanceRoot("`llz env add`")
	if err == nil {
		t.Fatal("expected a refusal below the instance root")
	}
	if !strings.Contains(err.Error(), "inside an instance, below its root") {
		t.Errorf("error %q should say the root is above, not suggest `llz new`", err)
	}
	if strings.Contains(err.Error(), "llz new") {
		t.Errorf("error %q suggests scaffolding a second instance", err)
	}
}
