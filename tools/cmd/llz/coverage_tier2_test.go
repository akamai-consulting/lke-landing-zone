package main

// Tier-2 coverage: functions that do real work but only touch the filesystem or
// environment, so a t.TempDir / t.Setenv / t.Chdir test covers them without any
// kubectl / API / subprocess mocking.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envdef"
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

func TestWriteEnvDefinition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environments", "prod.yaml")
	o := envdef.Opts{
		Region:          "us-ord",
		K8sVersion:      "1.31",
		NodeType:        "g6-standard-4",
		NodeCount:       "3",
		HARole:          "active",
		HAGroup:         "pair-1",
		PromotionRank:   2,
		RunnerIPv4CIDRs: "1.2.3.4/32",
		ObjCluster:      "us-ord-1",
	}
	if err := envdef.WriteEnvDefinition(path, "prod", o, "myinst"); err != nil {
		t.Fatalf("envdef.WriteEnvDefinition: %v", err)
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
	if err := envdef.WriteEnvDefinition(min, "dev", envdef.Opts{Region: "us-iad", ObjCluster: "us-iad-1"}, "myinst"); err != nil {
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
