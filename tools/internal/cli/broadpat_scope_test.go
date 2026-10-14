package cli

// broadpat_scope_test.go — the scope-parity test STAYED, and it is the one test
// that could not follow broad-PAT out.
//
// It asserts a RELATIONSHIP between two scope sets: the broad PAT's
// (internal/credrotate) and the in-cluster PAT's (ci_incluster_pat.go, still
// here). Only package main can see both, so this is the same call docsguard's six
// cobra tests and manifestguard's class-table tests made — a coupling test lives
// where the coupling is visible.
//
// It follows credrotate's the moment ci_incluster_pat.go does, which needs the
// OpenBao WRITE layer and the GitHub OIDC layer: the fourth wall.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
)

// parseScopes maps "resource:access" scope strings to an access level
// (read_only=1, read_write=2) for superset comparison.
func parseScopes(s string) map[string]int {
	m := map[string]int{}
	for _, f := range strings.Fields(s) {
		res, acc, ok := strings.Cut(f, ":")
		if !ok {
			continue
		}
		lvl := 1
		if acc == "read_write" {
			lvl = 2
		}
		m[res] = lvl
	}
	return m
}

// TestBroadPATScopesSupersetInclusterPAT guards the load-bearing invariant that
// broke the broad-pat e2e: the broad PAT (which the rotator publishes as each
// deployment's LINODE_API_TOKEN) must be able to mint the narrow in-cluster PAT.
// Linode rejects creating a token with scopes greater than the requesting token's,
// so credrotate.BroadPATScopes must cover every credrotate.InClusterPATScopes resource at >= its access.
func TestBroadPATScopesSupersetInclusterPAT(t *testing.T) {
	broad := parseScopes(credrotate.BroadPATScopes)
	for res, need := range parseScopes(credrotate.InClusterPATScopes) {
		got, ok := broad[res]
		if !ok {
			t.Errorf("credrotate.BroadPATScopes is missing %q (in-cluster PAT needs it) — mint-bootstrap-pat 400s after a rotation", res)
			continue
		}
		if got < need {
			t.Errorf("credrotate.BroadPATScopes has %q at level %d but in-cluster PAT needs %d", res, got, need)
		}
	}
}
