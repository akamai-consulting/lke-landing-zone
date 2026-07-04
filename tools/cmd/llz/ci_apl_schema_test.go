package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The templatefile() map keys are extracted from the real main.tf block shape.
func TestTemplatefileMapKeys(t *testing.T) {
	mainTF := `
locals {
  apl_rendered_values = templatefile(
    "${path.module}/../../apl-values/${var.apl_values_env}/values.yaml",
    {
      apl_values_repo_password = var.apl_values_repo_token
      linode_dns_token         = var.linode_dns_token
      coredns_cluster_ip       = try(data.kubernetes_service.coredns[0].spec[0].cluster_ip, "")
      loki_admin_password      = random_password.loki_admin.result
    }
  )
  other = 1
}
`
	keys := templatefileMapKeys(mainTF)
	want := []string{"apl_values_repo_password", "coredns_cluster_ip", "linode_dns_token", "loki_admin_password"}
	if got := sortedSetKeys(keys); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("templatefileMapKeys = %v, want %v", got, want)
	}
	// `other` (outside the templatefile block) must NOT be captured.
	if keys["other"] {
		t.Error("captured a key from outside the templatefile() block")
	}
}

// Unescaped ${var} not in the map is flagged; escaped $${var} and wired vars are not.
func TestUnwiredPlaceholders(t *testing.T) {
	keys := map[string]bool{"loki_admin_password": true, "coredns_cluster_ip": true, "loki_s3_endpoint": true}
	values := `
    adminPassword: ${loki_admin_password}      # wired → ok
    endpoint: ${loki_s3_endpoint}              # wired, has a digit → ok
    repoUrl: ${apl_values_repo_url}            # NOT in map → unwired
    # escaped, literal, ignored: $${coredns_cluster_ip}
    ip: ${bogus_var}                           # NOT in map → unwired
`
	got := unwiredPlaceholders(values, keys)
	want := []string{"apl_values_repo_url", "bogus_var"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unwiredPlaceholders = %v, want %v", got, want)
	}
}

func TestParseAplChartVersion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`apl_chart_version = "6.0.0"`, "6.0.0"},
		{"foo = 1\napl_chart_version   =   \"6.1.3\"\n", "6.1.3"},
		{`apl_chart_version = "latest"`, ""}, // not X.Y.Z
		{"no version here", ""},
	} {
		if got := parseAplChartVersion(tc.in); got != tc.want {
			t.Errorf("parseAplChartVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The var-contract check fails on an unwired placeholder before any helm call.
func TestRunValidateAplValuesVarContractFails(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	mainTF := filepath.Join(dir, "main.tf")
	mustWrite(t, values, "repoUrl: ${apl_values_repo_url}\n")
	mustWrite(t, mainTF, "apl_rendered_values = templatefile(\n  \"x\",\n  {\n    loki_admin_password = y\n  }\n  )\n")
	err := runValidateAplValues(values, "", mainTF, true) // skip schema
	if err == nil || !strings.Contains(err.Error(), "apl_values_repo_url") {
		t.Fatalf("want unwired-placeholder error, got %v", err)
	}
}

// Schema orchestration (hermetic — the helm exec is mocked, no real helm/PATH):
// a template failure surfaces helm's schema error; on success the pinned version
// flows through and placeholders are stubbed away before helm sees the file.
func TestValidateAplSchema(t *testing.T) {
	orig := helmRunner
	defer func() { helmRunner = orig }()

	helmRunner = func(args ...string) (string, bool) {
		if len(args) > 0 && args[0] == "template" {
			return "Error: at '/apps/loki': missing property 'adminPassword'", false
		}
		return "", true
	}
	if err := validateAplSchema("apps: {}", "6.0.0"); err == nil {
		t.Fatal("expected schema-violation error, got nil")
	}

	var usedVersion bool
	helmRunner = func(args ...string) (string, bool) {
		for i, a := range args {
			if a == "--version" && i+1 < len(args) && args[i+1] == "6.0.0" {
				usedVersion = true
			}
		}
		return "", true
	}
	if err := validateAplSchema("adminPassword: ${loki_admin_password}\n", "6.0.0"); err != nil {
		t.Fatalf("valid values should pass: %v", err)
	}
	if !usedVersion {
		t.Error("helm template did not receive the pinned version 6.0.0")
	}
}
