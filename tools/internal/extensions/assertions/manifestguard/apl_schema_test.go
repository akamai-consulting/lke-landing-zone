package manifestguard

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
)

// The runtime-placeholder set is exactly the secrets-only tokens bootstrap-cluster
// fills — the single source of truth this guard checks against. loki_admin_password
// left the set when apl-core 6.2.0 stopped requiring apps.loki.adminPassword; a
// placeholder nothing fills must not sit here claiming it is wired.
func TestPlaceholderSet(t *testing.T) {
	keys := placeholderSet()
	want := []string{"apl_values_repo_password", "coredns_cluster_ip", "linode_dns_token"}
	if got := cli.SortedKeys(keys); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("placeholderSet = %v, want %v", got, want)
	}
}

// Unescaped ${var} not in the map is flagged; escaped $${var} and wired vars are not.
func TestUnwiredPlaceholders(t *testing.T) {
	keys := map[string]bool{"linode_dns_token": true, "coredns_cluster_ip": true, "loki_s3_endpoint": true}
	values := `
    apiToken: ${linode_dns_token}              # wired → ok
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

// The var-contract check fails on an unwired placeholder before any helm call.
func TestRunValidateAplValuesVarContractFails(t *testing.T) {
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	mustWrite(t, values, "repoUrl: ${apl_values_repo_url}\n")
	err := RunValidateAplValues(values, "", true) // no chart version, skip schema
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
	if err := validateAplSchema("apiToken: ${linode_dns_token}\n", "6.0.0"); err != nil {
		t.Fatalf("valid values should pass: %v", err)
	}
	if !usedVersion {
		t.Error("helm template did not receive the pinned version 6.0.0")
	}
}

// The hint exists because the failure it explains is one THIS release causes: the
// delivered values base no longer renders apps.loki.adminPassword, which every
// chart below 6.2.0 still marks required. Without the hint helm's message
// ("adminPassword is required") sends the operator hunting for a missing secret
// instead of at the pin.
func TestAplVersionHint(t *testing.T) {
	const schemaErr = "Error: at '/apps/loki': missing property 'adminPassword'"

	if got := aplVersionHint(schemaErr, "6.1.0"); !strings.Contains(got, "6.2.0") || !strings.Contains(got, "aplChartVersion") {
		t.Errorf("a sub-6.2.0 pin must be named as the cause and the fix, got %q", got)
	}
	// At or above the cutoff the field is genuinely absent from `required`, so an
	// adminPassword error means something else entirely — do not misattribute it.
	if got := aplVersionHint(schemaErr, "v6.2.0"); got != "" {
		t.Errorf("no hint at/above the cutoff, got %q", got)
	}
	// Any other schema violation must stay unannotated; a guess here is worse
	// than silence.
	if got := aplVersionHint("Error: at '/apps/harbor': missing property 'foo'", "6.1.0"); got != "" {
		t.Errorf("unrelated schema errors must not be attributed to the pin, got %q", got)
	}
}
