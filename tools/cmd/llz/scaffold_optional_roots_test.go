package main

import (
	"path/filepath"
	"testing"
)

// TestOptionalTFVars pins which roots may be absent without readiness calling it a
// defect. `databases` is opt-in — an instance that never declares
// spec.cluster.databases never applies it — so a missing databases/<env>.tfvars is
// normal. cluster and object-storage are not: every instance needs both, and a
// legacy (pre-spec) instance missing one is a real finding readiness must keep
// reporting. Adding databases to tfRoots without this distinction would have made
// every legacy instance report a blocking "missing" for a database it never wanted.
func TestOptionalTFVars(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{filepath.Join("terraform-iac-bootstrap", "databases", "prod.tfvars"), true},
		{filepath.Join("terraform-iac-bootstrap", "cluster", "prod.tfvars"), false},
		{filepath.Join("terraform-iac-bootstrap", "object-storage", "prod.tfvars"), false},
	} {
		if got := optionalTFVars(tc.path); got != tc.want {
			t.Errorf("optionalTFVars(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	// Every optional root must be a real root, or the set is silently inert.
	for name := range optionalTFRoots {
		var found bool
		for _, r := range tfRoots {
			if r == name {
				found = true
			}
		}
		if !found {
			t.Errorf("optionalTFRoots names %q, which is not in tfRoots %v", name, tfRoots)
		}
	}
}
