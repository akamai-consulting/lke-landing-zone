package instancelayout

// TestInstanceLayout followed Detect here. It was the SEVENTH test this branch has
// found stranded in package main's coverage_tier1_test.go — a file named for a
// coverage TIER, which is why nothing about it ever indicates which code it covers.

import (
	"os"
	"testing"
)

func TestInstanceLayout(t *testing.T) {
	t.Chdir(t.TempDir())

	// Rendered instance: roots at repo root.
	tf, apl, prefix := Detect()
	if tf != "terraform-iac-bootstrap" || apl != "apl-values" || prefix != "" {
		t.Errorf("rendered layout = (%q,%q,%q)", tf, apl, prefix)
	}

	// Template-repo checkout: roots under instance-template/.
	if err := os.MkdirAll("instance-template/terraform-iac-bootstrap", 0o755); err != nil {
		t.Fatal(err)
	}
	tf, apl, prefix = Detect()
	if tf != "instance-template/terraform-iac-bootstrap" || apl != "instance-template/apl-values" || prefix != "instance-template/" {
		t.Errorf("template layout = (%q,%q,%q)", tf, apl, prefix)
	}
}
