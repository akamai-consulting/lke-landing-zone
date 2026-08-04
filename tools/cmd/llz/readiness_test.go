package main

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
	got, present := scanForSentinels(f, true)
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
	if _, present := scanForSentinels(filepath.Join(dir, "nope.tfvars"), true); present {
		t.Error("scanForSentinels reported a missing file as present")
	}
}

func TestHintForInstanceRepoDependsOnWhereItWasFound(t *testing.T) {
	// Same placeholder, two different fixes. In a RENDERED overlay file it comes
	// from spec.instance.repo, so hand-editing it is undone by the next render —
	// and `llz env add` lists six or more such files, which is a lot of wasted
	// editing to then lose. In the chart values (adopter-guide §5) it really is a
	// hand edit, because nothing renders those.
	s := sentinel{instanceRepoPlaceholder, true, "repoint to your fork / instance repo (owner/name)"}

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

func TestGroupFindings(t *testing.T) {
	// Nine files, one fix. Collapsing by (token, hint) is what makes the checklist
	// agree with the hint it prints: `llz spec set` once, not nine hand edits.
	in := []finding{
		{file: "a.yaml", line: 1, token: instanceRepoPlaceholder, hint: "spec fix"},
		{file: "b.yaml", line: 2, token: instanceRepoPlaceholder, hint: "spec fix"},
		{file: "c.yaml", line: 3, token: "REPLACE_ME", hint: "hand edit"},
		{file: "d.yaml", line: 4, token: instanceRepoPlaceholder, hint: "hand edit"}, // same token, DIFFERENT remedy
	}
	got := groupFindings(in)
	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3 (same token with a different remedy must not merge): %+v", len(got), got)
	}
	if got[0].files != 2 || got[0].first.file != "a.yaml" {
		t.Errorf("first group = %+v, want 2 files starting at a.yaml", got[0])
	}
	if got[1].files != 1 || got[1].first.token != "REPLACE_ME" {
		t.Errorf("second group = %+v, want the single REPLACE_ME", got[1])
	}
	if got[2].files != 1 {
		t.Errorf("third group = %+v, want 1", got[2])
	}
	if groupFindings(nil) != nil {
		t.Error("no findings should group to nothing")
	}
}

func TestGroupFindingsCountsFilesNotOccurrences(t *testing.T) {
	// One file can carry the same placeholder twice (instance-custom.yaml does),
	// and the count is printed as "(+N more file(s))". Counting occurrences
	// inflated it — the checklist would claim more files than exist.
	in := []finding{
		{file: "a.yaml", line: 1, token: instanceRepoPlaceholder, hint: "h"},
		{file: "a.yaml", line: 9, token: instanceRepoPlaceholder, hint: "h"}, // same file, second line
		{file: "b.yaml", line: 3, token: instanceRepoPlaceholder, hint: "h"},
	}
	got := groupFindings(in)
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(got), got)
	}
	if got[0].files != 2 {
		t.Errorf("files = %d, want 2 (a.yaml and b.yaml — not 3 occurrences)", got[0].files)
	}
	if n := countFiles(in); n != 2 {
		t.Errorf("countFiles = %d, want 2", n)
	}
}
