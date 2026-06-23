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

// #1: a bad/unknown path is rejected and the file is left untouched (not poisoned).
func TestEditSpecFileRollback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lab.yaml")
	orig := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata: { name: lab }\nspec:\n  cluster:\n    region: us-ord\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	parse := func(b []byte) error { _, e := clusterspec.DecodeClusterDefinition(b); return e }

	err := editSpecFile(p, func(doc *yaml.Node) error { return setSpecPath(doc, "cluster.nodePol.kount", "9") }, parse)
	if err == nil || !strings.Contains(err.Error(), "left unchanged") {
		t.Fatalf("expected a reverted rejection, got: %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Errorf("file mutated despite rejection:\n%s", got)
	}
	// A valid field commits.
	if err := editSpecFile(p, func(doc *yaml.Node) error { return setSpecPath(doc, "cluster.k8sVersion", "v1.33.6+lke7") }, parse); err != nil {
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
		if got := isPerEnvPath(p); got != want {
			t.Errorf("isPerEnvPath(%q) = %v, want %v", p, got, want)
		}
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
// committedTargets emits only the THIN per-env files (overlay + env-revision +
// region patch + values) — the manifests themselves live in the shared base.
func TestCommittedTargets(t *testing.T) {
	chdirTempDir(t)
	writeFileMkdir(t, filepath.Join("apl-values", "_shared", "values.yaml"), "apps:\n  harbor: { enabled: true }\n")
	e := clusterspec.Environment{Components: map[string]clusterspec.ComponentToggle{}} // all default-enabled

	targets, err := committedTargets("lab", e, clusterspec.ValuesIdentity{ClusterName: "x"}, "apl-values")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join("apl-values", "lab", "manifest", "kustomization.yaml"),
		filepath.Join("apl-values", "lab", "manifest", "env-revision-configmap.yaml"),
		filepath.Join("apl-values", "lab", "manifest", "linode-volume-labeler-region-patch.yaml"), // volumeLabeler default-on
		filepath.Join("apl-values", "lab", "values.yaml"),
	} {
		if _, ok := targets[p]; !ok {
			t.Errorf("missing committed target %s", p)
		}
	}
	overlay := targets[filepath.Join("apl-values", "lab", "manifest", "kustomization.yaml")]
	if !strings.Contains(overlay, "../../_shared/manifest") {
		t.Errorf("overlay is not thin (no shared base ref):\n%s", overlay)
	}
	// volumeLabeler disabled → no region patch target.
	off := clusterspec.Environment{Components: map[string]clusterspec.ComponentToggle{"volumeLabeler": {Enabled: boolPtrLocal(false)}}}
	t2, _ := committedTargets("lab", off, clusterspec.ValuesIdentity{}, "apl-values")
	if _, ok := t2[filepath.Join("apl-values", "lab", "manifest", "linode-volume-labeler-region-patch.yaml")]; ok {
		t.Error("disabled volumeLabeler should not emit a region patch")
	}
}

func boolPtrLocal(b bool) *bool { return &b }

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
