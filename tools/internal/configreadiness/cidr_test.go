package configreadiness

// Followed isOpenWorldCIDRLine here out of package main's branch_coverage_test.go —
// another file named for a COVERAGE METRIC rather than a subject. Sixth time.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

func TestIsOpenWorldCIDRLine(t *testing.T) {
	for _, open := range []string{
		`github_runner_ipv4_cidrs = ["0.0.0.0/0"]`,
		`github_runner_ipv4_cidrs = ["203.0.113.0/24", "0.0.0.0/0"]`,
		`github_runner_ipv6_cidrs = ["::/0"]`,
	} {
		if !isOpenWorldCIDRLine(open) {
			t.Errorf("isOpenWorldCIDRLine(%q) = false, want true", open)
		}
	}
	for _, ok := range []string{
		`github_runner_ipv4_cidrs = ["203.0.113.0/24"]`,
		`github_runner_ipv4_cidrs = []`,
		`github_runner_ipv4_cidrs = [] # was "0.0.0.0/0"`, // the comment is not the value
		`node_count = 5`,
		`github_runner_ipv4_cidrs`, // no assignment at all
	} {
		if isOpenWorldCIDRLine(ok) {
			t.Errorf("isOpenWorldCIDRLine(%q) = true, want false", ok)
		}
	}
}

func TestRunEnvReadinessOpenWorldACL(t *testing.T) {
	// `llz env add` rejects 0.0.0.0/0 at the flag, but a spec is a file: `llz env
	// edit`, a hand edit, or an inherited spec.defaults renders one without ever
	// passing that check. doctor reports it — as a finding, not a blocker, so an
	// instance that already has one can still render and build while it fixes it.
	dir := chdirTempDir(t)
	writeGoodReadiness(t, dir, "e2e")
	writeTFVars(t, dir, "cluster", "e2e",
		"region = \"us-ord\"\ngithub_runner_ipv4_cidrs = [\"0.0.0.0/0\"]\n")
	var err error
	out := captureStdout(t, func() { err = RunEnvReadiness("e2e") })
	if err != nil {
		t.Fatalf("an open ACL must be reported, not blocking: %v\n%s", err, out)
	}
	if !strings.Contains(out, "admits every address") {
		t.Errorf("open-world ACL not flagged:\n%s", out)
	}
}

func TestOpenWorldACLFindings(t *testing.T) {
	// The spec-level half of the ACL check. It exists because the other two paths
	// structurally cannot see this: `llz env add` validates only the FLAGS it was
	// given, and the tfvars scan reads a gitignored build artifact that a fresh
	// clone has not rendered. The spec is merged at load (applyInheritance), so an
	// open-world prefix inherited from spec.defaults — which environments/<env>.yaml
	// never even mentions — arrives here like an explicit one.
	got := openWorldACLFindings("lab", clusterspec.AllowCIDRs{
		IPv4: []string{"203.0.113.0/24", "0.0.0.0/0"},
		IPv6: []string{"2001:db8::/32", "::/0"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (one per open prefix): %+v", len(got), got)
	}
	for _, f := range got {
		if f.Blocking {
			t.Errorf("%q must be a finding, not a blocker — an instance that already has one still has to be able to build while it fixes it", f.Token)
		}
		// Named against the spec, not a rendered artifact: that is where the
		// operator edits, and for an inherited value the env file has no such line.
		if f.File != filepath.Join("environments", "lab.yaml") {
			t.Errorf("finding points at %q, want the env spec file", f.File)
		}
		if !strings.Contains(f.Hint, "if it is inherited") {
			t.Errorf("hint should send the operator to spec.defaults too: %q", f.Hint)
		}
	}
	// A closed ACL says nothing.
	if got := openWorldACLFindings("lab", clusterspec.AllowCIDRs{IPv4: []string{"203.0.113.0/24"}}); len(got) != 0 {
		t.Errorf("a closed ACL must produce no findings, got %+v", got)
	}
}
