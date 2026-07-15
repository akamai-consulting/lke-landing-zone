package tfroots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoots(t *testing.T) {
	want := []string{"cluster", "cluster-bootstrap", "object-storage", "vpc"}
	got := Roots()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Roots() = %v, want %v", got, want)
	}
}

func TestRenderWritesTokenFreeTF(t *testing.T) {
	dst := t.TempDir()
	written, err := Render(dst, "akamai-consulting", "v9.9.9", "acme/inst")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(written) == 0 {
		t.Fatal("Render wrote nothing")
	}

	// Every root must land its full *.tf set (backend.tf + versions.tf among them),
	// every written file must be a .tf under the right root, and no copier token may
	// survive substitution.
	byRoot := map[string]map[string]bool{}
	for _, p := range written {
		if !strings.HasSuffix(p, ".tf") {
			t.Errorf("Render wrote a non-.tf file: %s", p)
		}
		rel, err := filepath.Rel(filepath.Join(dst, "terraform-iac-bootstrap"), p)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		parts := strings.SplitN(rel, string(filepath.Separator), 2)
		if byRoot[parts[0]] == nil {
			byRoot[parts[0]] = map[string]bool{}
		}
		byRoot[parts[0]][parts[1]] = true

		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if strings.Contains(string(b), "<@") {
			t.Errorf("%s still has an unsubstituted copier token:\n%s", p, b)
		}
	}

	for _, root := range Roots() {
		files := byRoot[root]
		if files == nil {
			t.Errorf("root %q produced no .tf files", root)
			continue
		}
		for _, must := range []string{"backend.tf", "versions.tf", "main.tf"} {
			if !files[must] {
				t.Errorf("root %q missing %s", root, must)
			}
		}
	}

	// backend.tf is static (no token) → substitution leaves it identical to the embed.
	rawBackend, err := embedded.ReadFile("roots/cluster/backend.tf")
	if err != nil {
		t.Fatal(err)
	}
	gotBackend, err := os.ReadFile(filepath.Join(dst, "terraform-iac-bootstrap", "cluster", "backend.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rawBackend) != string(gotBackend) {
		t.Errorf("static backend.tf changed under substitution")
	}

	// A token-bearing file must reflect the substituted values.
	clusterMain, err := os.ReadFile(filepath.Join(dst, "terraform-iac-bootstrap", "cluster", "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(clusterMain), "akamai-consulting/lke-landing-zone.git//terraform-modules/llz-cluster?ref=v9.9.9") {
		t.Errorf("cluster/main.tf tokens not substituted:\n%s", clusterMain)
	}
}

func TestRenderSkipsExampleAndReadme(t *testing.T) {
	dst := t.TempDir()
	written, err := Render(dst, "o", "r", "i")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, p := range written {
		if strings.HasSuffix(p, "terraform.tfvars.example") {
			t.Errorf("Render must not write a tfvars.example: %s", p)
		}
		if strings.HasSuffix(p, "README.md") {
			t.Errorf("Render must not write a README.md: %s", p)
		}
	}
	// And they must not exist on disk at all.
	if _, err := os.Stat(filepath.Join(dst, "terraform-iac-bootstrap", "cluster", "terraform.tfvars.example")); !os.IsNotExist(err) {
		t.Errorf("tfvars.example was written to disk (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "terraform-iac-bootstrap", "cluster-bootstrap", "README.md")); !os.IsNotExist(err) {
		t.Errorf("README.md was written to disk (err=%v)", err)
	}
}

func TestTargetsMatchesRender(t *testing.T) {
	dst := t.TempDir()
	targets := Targets(dst)
	written, err := Render(dst, "o", "r", "i")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Join(targets, "\n") != strings.Join(written, "\n") {
		t.Errorf("Targets != Render written set\ntargets: %v\nwritten: %v", targets, written)
	}
}

func TestFilesMatchesTargetsAndRender(t *testing.T) {
	dst := t.TempDir()
	f := Files(dst, "akamai-consulting", "v1", "acme/inst")
	// Keys == Targets, values are token-free.
	for _, p := range Targets(dst) {
		content, ok := f[p]
		if !ok {
			t.Errorf("Files missing target %s", p)
		}
		if strings.Contains(content, "<@") {
			t.Errorf("Files[%s] has an unsubstituted token", p)
		}
	}
	if len(f) != len(Targets(dst)) {
		t.Errorf("Files has %d entries, Targets has %d", len(f), len(Targets(dst)))
	}
}

func TestTfvarsExample(t *testing.T) {
	for _, root := range Roots() {
		b, err := TfvarsExample(root)
		if err != nil {
			t.Errorf("TfvarsExample(%q): %v", root, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("TfvarsExample(%q) is empty", root)
		}
	}
	// cluster-bootstrap's example carries the instance_repo token verbatim (raw bytes).
	b, err := TfvarsExample("cluster-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), tokInstanceRepo) {
		t.Errorf("cluster-bootstrap tfvars.example should keep the raw instance_repo token")
	}
	if _, err := TfvarsExample("nope"); err == nil {
		t.Errorf("TfvarsExample of a bogus root should error")
	}
}

func TestRenderMkdirError(t *testing.T) {
	dst := t.TempDir()
	// Block directory creation: make terraform-iac-bootstrap a regular file so
	// Render's MkdirAll under it fails.
	if err := os.WriteFile(filepath.Join(dst, "terraform-iac-bootstrap"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(dst, "o", "r", "i"); err == nil {
		t.Errorf("Render should fail when it cannot create the root dirs")
	}
}

func TestRenderWriteError(t *testing.T) {
	dst := t.TempDir()
	// Occupy a target path with a directory so os.WriteFile fails for that file.
	blocked := filepath.Join(dst, "terraform-iac-bootstrap", "cluster-bootstrap", "backend.tf")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(dst, "o", "r", "i"); err == nil {
		t.Errorf("Render should fail when a target path is not writable")
	}
}

func TestSubstitute(t *testing.T) {
	in := "org=<@ upstream_org @> ref=<@ llz_version @> repo=<@ instance_repo @>"
	got := Substitute(in, "akamai-consulting", "v1.2.3", "acme/inst")
	want := "org=akamai-consulting ref=v1.2.3 repo=acme/inst"
	if got != want {
		t.Errorf("Substitute = %q, want %q", got, want)
	}
}
