package main

// Tier-2 coverage: functions that do real work but only touch the filesystem or
// environment, so a t.TempDir / t.Setenv / t.Chdir test covers them without any
// kubectl / API / subprocess mocking.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWriteEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.env")
	if err := writeEnvFile(path, map[string]string{"A": "1", "B": "two"}); err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	got := readEnvFile(path) // round-trips through the sibling reader
	if got["A"] != "1" || got["B"] != "two" {
		t.Errorf("round-trip = %v", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
	}
}

func TestEditYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte("cluster:\n  name: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := editYAMLFile(path, func(doc *yaml.Node) error {
		setScalarChild(doc.Content[0], "added", "yes")
		return nil
	})
	if err != nil {
		t.Fatalf("editYAMLFile: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "added: yes") {
		t.Errorf("mutation not written:\n%s", b)
	}

	// Error paths.
	if err := editYAMLFile(filepath.Join(dir, "nope.yaml"), func(*yaml.Node) error { return nil }); err == nil {
		t.Error("missing file should error")
	}
	empty := filepath.Join(dir, "empty.yaml")
	os.WriteFile(empty, nil, 0o644)
	if err := editYAMLFile(empty, func(*yaml.Node) error { return nil }); err == nil {
		t.Error("empty doc should error")
	}
	sentinel := errors.New("mutate failed")
	if err := editYAMLFile(path, func(*yaml.Node) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("mutate error should propagate, got %v", err)
	}
}

func TestWriteEnvDefinition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environments", "prod.yaml")
	o := envAddOpts{
		region:          "us-ord",
		k8sVersion:      "1.31",
		nodeType:        "g6-standard-4",
		nodeCount:       "3",
		haRole:          "active",
		haGroup:         "pair-1",
		promotionRank:   2,
		runnerIPv4CIDRs: "1.2.3.4/32",
		objCluster:      "us-ord-1",
	}
	if err := writeEnvDefinition(path, "prod", o, "myinst"); err != nil {
		t.Fatalf("writeEnvDefinition: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"name: prod",
		"clusterLabel: myinst-prod",
		"region: us-ord",
		"k8sVersion: 1.31",
		"nodePool: { type: g6-standard-4, count: 3 }",
		"role: active",
		"group: pair-1",
		"promotionRank: 2",
		"name: myinst-prod",
		"cluster: us-ord-1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("env definition missing %q:\n%s", want, s)
		}
	}
	// On the managed platform Linode owns the domain — the env must NOT author a
	// domainSuffix (managedAppPlatform is inherited from spec.defaults).
	if strings.Contains(s, "domainSuffix") {
		t.Errorf("env definition must NOT author domainSuffix (managed owns the domain):\n%s", s)
	}

	// Minimal opts: optional blocks omitted, role defaults to standalone.
	min := filepath.Join(t.TempDir(), "dev.yaml")
	if err := writeEnvDefinition(min, "dev", envAddOpts{region: "us-iad", objCluster: "us-iad-1"}, "myinst"); err != nil {
		t.Fatal(err)
	}
	mb, _ := os.ReadFile(min)
	if strings.Contains(string(mb), "k8sVersion") || strings.Contains(string(mb), "nodePool") {
		t.Errorf("unset optional fields should be omitted:\n%s", mb)
	}
	if !strings.Contains(string(mb), "role: standalone") {
		t.Errorf("default role should be standalone:\n%s", mb)
	}
}
