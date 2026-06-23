package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	yaml "gopkg.in/yaml.v3"
)

// #1: setSpecPath edits nested fields with correct scalar typing + comment survival.
func TestSetSpecPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(p, []byte("kind: ClusterDefinition\nspec:\n  cluster:\n    region: us-ord  # keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := editYAMLFile(p, func(doc *yaml.Node) error {
		for _, a := range [][2]string{
			{"cluster.nodePool.count", "8"},
			{"components.harbor.enabled", "false"},
			{"components.observability.retention", "30d"},
		} {
			if err := setSpecPath(doc, a[0], a[1]); err != nil {
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

func TestParseAssignments(t *testing.T) {
	got, err := parseAssignments([]string{"a.b=c", "x= y "})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != [2]string{"a.b", "c"} || got[1] != [2]string{"x", "y"} {
		t.Errorf("parseAssignments = %v", got)
	}
	if _, err := parseAssignments([]string{"noequals"}); err == nil {
		t.Error("expected error for an arg without '='")
	}
}

func TestInferScalarTag(t *testing.T) {
	for v, want := range map[string]string{"true": "!!bool", "false": "!!bool", "8": "!!int", "30d": "!!str", "us-ord": "!!str", "10.0.0.0/14": "!!str"} {
		if got := inferScalarTag(v); got != want {
			t.Errorf("inferScalarTag(%q) = %q, want %q", v, got, want)
		}
	}
}

// #3: an HA group with only one peer reports the missing role; complete → "".
func TestHaGroupMissingRole(t *testing.T) {
	chdirTempDir(t)
	writeSpecInstance(t, map[string]string{
		"east": clusterDef("east", "    ha: { role: active, group: prod }\n"),
	})
	if got := haGroupMissingRole("prod"); got != "standby" {
		t.Errorf("missing role with only active = %q, want standby", got)
	}
	writeFileMkdir(t, filepath.Join("environments", "west.yaml"), clusterDef("west", "    ha: { role: standby, group: prod }\n"))
	if got := haGroupMissingRole("prod"); got != "" {
		t.Errorf("complete pair should report no missing role, got %q", got)
	}
}

// #4: committedTargets renders the letsencrypt issuer with the spec ACME email,
// and omits it entirely when no email is set.
func TestCommittedTargetsAcmeEmail(t *testing.T) {
	chdirTempDir(t)
	rel := filepath.Join("apl-values", "example", "manifest", "dns", "letsencrypt-clusterissuer.yaml")
	writeFileMkdir(t, rel, "spec:\n  acme:\n    email: REPLACE_PER_ENV\n")
	want := filepath.Join("apl-values", "lab", "manifest", "dns", "letsencrypt-clusterissuer.yaml")

	with, err := committedTargets("lab", nil, clusterspec.ValuesIdentity{}, "ops@acme.io", "apl-values")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with[want], "email: ops@acme.io") {
		t.Errorf("letsencrypt not rendered with the email:\n%s", with[want])
	}
	without, _ := committedTargets("lab", nil, clusterspec.ValuesIdentity{}, "", "apl-values")
	if _, ok := without[want]; ok {
		t.Error("no acmeEmail should not produce a letsencrypt target")
	}
}

// #9: the LCS diff shows scattered changes as separate hunks with a collapse marker.
func TestLineDiffScattered(t *testing.T) {
	old := "x\n1\n2\n3\n4\n5\n6\n7\ny\n"
	new := "X\n1\n2\n3\n4\n5\n6\n7\nY\n"
	d := lineDiff(old, new)
	if !strings.Contains(d, "- x") || !strings.Contains(d, "+ X") || !strings.Contains(d, "- y") || !strings.Contains(d, "+ Y") {
		t.Errorf("both hunks should appear:\n%s", d)
	}
	if !strings.Contains(d, "…") {
		t.Errorf("unchanged run between hunks should collapse to …:\n%s", d)
	}
}
