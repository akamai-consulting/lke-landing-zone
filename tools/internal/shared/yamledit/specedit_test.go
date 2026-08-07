package yamledit

// FOUR TESTS FROM A FILE CALLED env_set_test.go, WHICH CONTAINED NO TESTS FOR
// env_set.go AT ALL.
//
// Every function in it tested this package (SetSpecPath, EditSpecFile,
// IsPerEnvPath, ParseAssignments) or package main (lineDiff). The file was named
// for the command whose implementation happened to call them, which is the same
// mis-filing as the coverage_tier* files — different cause, identical effect: a
// test whose location says nothing about its subject.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"gopkg.in/yaml.v3"
)

func TestSetSpecPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(p, []byte("kind: ClusterDefinition\nspec:\n  cluster:\n    region: us-ord  # keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := EditYAMLFile(p, func(doc *yaml.Node) error {
		for _, a := range [][2]string{
			{"cluster.nodePool.count", "8"},
			{"components.harbor.enabled", "false"},
			{"components.observability.retention", "30d"},
		} {
			if err := SetSpecPath(doc, a[0], a[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("editYAMLFile: %v", err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, want := range []string{"count: 8", "enabled: false", "retention: 30d", "keep me"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, `count: "8"`) || strings.Contains(s, `enabled: "false"`) {
		t.Errorf("int/bool wrongly quoted:\n%s", s)
	}
	// retention 30d must be a string (quoted or plain, but parseable as such).
	if strings.Contains(s, "retention: 30") && !strings.Contains(s, "retention: 30d") {
		t.Errorf("retention mangled:\n%s", s)
	}
}

// #1: a bad/unknown path is rejected and the file is left untouched (not poisoned).
func TestEditSpecFileRollback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lab.yaml")
	orig := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata: { name: lab }\nspec:\n  cluster:\n    region: us-ord\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	parse := func(b []byte) error { _, e := clusterspec.DecodeClusterDefinition(b); return e }

	err := EditSpecFile(p, func(doc *yaml.Node) error { return SetSpecPath(doc, "cluster.nodePol.kount", "9") }, parse)
	if err == nil || !strings.Contains(err.Error(), "left unchanged") {
		t.Fatalf("expected a reverted rejection, got: %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Errorf("file mutated despite rejection:\n%s", got)
	}
	// A valid field commits.
	if err := EditSpecFile(p, func(doc *yaml.Node) error { return SetSpecPath(doc, "cluster.k8sVersion", "v1.33.6+lke7") }, parse); err != nil {
		t.Fatalf("valid set rejected: %v", err)
	}
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "k8sVersion: v1.33.6+lke7") {
		t.Errorf("valid set not applied:\n%s", got)
	}
}

// #2: per-env vs instance-level path classification (drives env set / spec set routing).
func TestIsPerEnvPath(t *testing.T) {
	for p, want := range map[string]bool{
		"cluster.region": true, "components.harbor.enabled": true,
		"dns.acmeEmail": false, "defaults.cluster.k8sVersion": false, "networks.x.region": false,
	} {
		if got := IsPerEnvPath(p); got != want {
			t.Errorf("isPerEnvPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestParseAssignments(t *testing.T) {
	got, err := ParseAssignments([]string{"a.b=c", "x= y "})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != [2]string{"a.b", "c"} || got[1] != [2]string{"x", "y"} {
		t.Errorf("parseAssignments = %v", got)
	}
	if _, err := ParseAssignments([]string{"noequals"}); err == nil {
		t.Error("expected error for an arg without '='")
	}
}
