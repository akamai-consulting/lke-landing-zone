package instancelayout

// The optional-roots test followed its table here.
//
// It asserts the rule optionalRoots encodes: `databases` is a root an instance may
// legitimately never apply, so a MISSING databases/<env>.tfvars is not a defect.
// Without it, adding `databases` to Roots would have every legacy (pre-spec)
// instance reporting a blocking "missing databases/<env>.tfvars — run llz env add"
// for a database they never asked for.

import (
	"path/filepath"
	"testing"
)

func TestOptionalTFVars(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{filepath.Join("terraform-iac-bootstrap", "databases", "prod.tfvars"), true},
		{filepath.Join("terraform-iac-bootstrap", "cluster", "prod.tfvars"), false},
		{filepath.Join("terraform-iac-bootstrap", "object-storage", "prod.tfvars"), false},
	} {
		if got := Optional(tc.path); got != tc.want {
			t.Errorf("optionalTFVars(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	// Every optional root must be a real root, or the set is silently inert.
	for name := range optionalRoots {
		var found bool
		for _, r := range Roots {
			if r == name {
				found = true
			}
		}
		if !found {
			t.Errorf("optionalRoots names %q, which is not in tfRoots %v", name, Roots)
		}
	}
}
