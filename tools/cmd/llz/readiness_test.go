package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsEmptyCIDRList(t *testing.T) {
	empty := []string{
		"github_runner_ipv4_cidrs = []",
		"  github_runner_ipv6_cidrs = []",
		`github_runner_ipv4_cidrs = []  # e.g. ["203.0.113.0/24"]`,
	}
	for _, l := range empty {
		if !isEmptyCIDRList(l) {
			t.Errorf("isEmptyCIDRList(%q) = false, want true", l)
		}
	}
	full := []string{
		`github_runner_ipv4_cidrs = ["203.0.113.0/24"]`,
		`cluster_label = "x"`,
		"# github_runner_ipv4_cidrs = []",
	}
	for _, l := range full {
		if isEmptyCIDRList(l) {
			t.Errorf("isEmptyCIDRList(%q) = true, want false", l)
		}
	}
}

func TestIsDeferrable(t *testing.T) {
	// cert/DNS overlay placeholders are deferred to `llz bootstrap dns` (post-build),
	// so they must not block the apply…
	deferred := []string{
		"apl-values/_shared/manifest/dns/letsencrypt-clusterissuer.yaml",
		"apl-values/_shared/manifest/dns/dns01-solver-externalsecret.yaml",
	}
	for _, f := range deferred {
		if !isDeferrable(f) {
			t.Errorf("isDeferrable(%q) = false, want true", f)
		}
	}
	// …while everything else (tfvars, non-dns overlay) still blocks.
	blocking := []string{
		"terraform-iac-bootstrap/cluster/lab.tfvars",
		"apl-values/lab/manifest/apps/some-app.yaml",
		"kubernetes-charts/llz-cert-automation/values.yaml",
	}
	for _, f := range blocking {
		if isDeferrable(f) {
			t.Errorf("isDeferrable(%q) = true, want false", f)
		}
	}
}

func TestScanForSentinels(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "cluster.tfvars")
	body := `apl_values_repo_url = "your-org/your-instance-repo"
# REPLACE_PER_ENV: this is documentation, must be ignored
github_runner_ipv4_cidrs = []
k8s_version = "v1.33.6+lke7"
`
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, present := scanForSentinels(f)
	if !present {
		t.Fatal("scanForSentinels reported file absent")
	}
	var blocking, warn int
	for _, fd := range got {
		if fd.blocking {
			blocking++
		} else {
			warn++
		}
	}
	if blocking != 1 {
		t.Errorf("blocking findings = %d, want 1 (%+v)", blocking, got)
	}
	if warn != 1 {
		t.Errorf("warn findings = %d, want 1 (%+v)", warn, got)
	}
	if _, present := scanForSentinels(filepath.Join(dir, "nope.tfvars")); present {
		t.Error("scanForSentinels reported a missing file as present")
	}
}
