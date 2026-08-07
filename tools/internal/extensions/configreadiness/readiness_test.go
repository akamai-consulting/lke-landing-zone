package configreadiness

import (
	"os"
	"path/filepath"
	"strings"
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
	// cert/DNS overlay placeholders are deferred (settable post-build; the
	// Argo-synced letsencrypt ClusterIssuers pick them up), so they must not block the apply…
	deferred := []string{
		"platform-apl/manifest/dns/letsencrypt-clusterissuer.yaml",
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
	got, present := ScanForSentinels(f, true)
	if !present {
		t.Fatal("ScanForSentinels reported file absent")
	}
	var blocking, warn int
	for _, fd := range got {
		if fd.Blocking {
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
	if _, present := ScanForSentinels(filepath.Join(dir, "nope.tfvars"), true); present {
		t.Error("ScanForSentinels reported a missing file as present")
	}
}

func TestHintForInstanceRepoDependsOnWhereItWasFound(t *testing.T) {
	// Same placeholder, two different fixes. In a RENDERED overlay file it comes
	// from spec.instance.repo, so hand-editing it is undone by the next render —
	// and `llz env add` lists six or more such files, which is a lot of wasted
	// editing to then lose. In the chart values (adopter-guide §5) it really is a
	// hand edit, because nothing renders those.
	s := sentinel{InstanceRepoPlaceholder, true, "repoint to your fork / instance repo (owner/name)"}

	dir := chdirTempDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}

	rendered := hintFor(s, filepath.Join("apl-values", "lab", "manifest", "llz-harbor.yaml"))
	if !strings.Contains(rendered, "llz spec set instance.repo=") {
		t.Errorf("a rendered file should point at the spec, got %q", rendered)
	}
	if !strings.Contains(rendered, "undone by the next `llz render`") {
		t.Errorf("a rendered file should warn that a hand edit is lost, got %q", rendered)
	}

	handEdited := hintFor(s, filepath.Join("kubernetes-charts", "llz-argo-bootstrap-apps", "values.yaml"))
	if handEdited != s.hint {
		t.Errorf("a non-rendered file keeps the hand-edit hint, got %q", handEdited)
	}

	// Other sentinels are untouched.
	other := sentinel{"REPLACE_ME", true, "replace the placeholder"}
	if got := hintFor(other, filepath.Join("apl-values", "lab", "x.yaml")); got != other.hint {
		t.Errorf("unrelated sentinel rewritten: %q", got)
	}
}
