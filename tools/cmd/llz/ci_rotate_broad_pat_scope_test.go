package main

import (
	"strings"
	"testing"
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
// so broadPATScopes must cover every inclusterPATScopes resource at >= its access.
func TestBroadPATScopesSupersetInclusterPAT(t *testing.T) {
	broad := parseScopes(broadPATScopes)
	for res, need := range parseScopes(inclusterPATScopes) {
		got, ok := broad[res]
		if !ok {
			t.Errorf("broadPATScopes is missing %q (in-cluster PAT needs it) — mint-bootstrap-pat 400s after a rotation", res)
			continue
		}
		if got < need {
			t.Errorf("broadPATScopes has %q at level %d but in-cluster PAT needs %d", res, got, need)
		}
	}
}
