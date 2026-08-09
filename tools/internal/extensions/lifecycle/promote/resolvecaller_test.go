package promote

// TestResolveCaller followed resolveCaller here out of package main's
// resolvecaller_test.go — another file named for a coverage TIER rather than a
// subject, which is now the fifth time that naming has hidden a test from its
// own code.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCaller(t *testing.T) {
	t.Chdir(t.TempDir())
	wfDir := "workflows"
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No rendered caller + no answers/stamp → error.
	if _, err := resolveCaller(testDeps(), wfDir); err == nil {
		t.Error("expected error when no pin source exists")
	}

	// A rendered promote.yml with a LEGACY cross-repo pin → preserved verbatim
	// (an old instance has no vendored body to point a local uses: at).
	promote := "jobs:\n  x:\n    uses: myorg/lke-landing-zone/.github/workflows/llz-terraform.yml@v2.3.4\n" +
		"    with:\n      instance_repo: myorg/inst\n"
	if err := os.WriteFile(filepath.Join(wfDir, "promote.yml"), []byte(promote), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := resolveCaller(testDeps(), wfDir)
	if err != nil {
		t.Fatalf("resolveCaller: %v", err)
	}
	if !strings.Contains(c.uses, "@v2.3.4") || c.instanceRepo != "myorg/inst" {
		t.Errorf("caller = %+v", c)
	}

	// An ADR-0003 instance's local uses: wins over the legacy fallback.
	local := "jobs:\n  x:\n    uses: ./.github/workflows/llz-terraform.yml\n" +
		"    with:\n      instance_repo: myorg/inst\n"
	if err := os.WriteFile(filepath.Join(wfDir, "promote.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = resolveCaller(testDeps(), wfDir)
	if err != nil {
		t.Fatalf("resolveCaller (local): %v", err)
	}
	if c.uses != localTerraformUses || c.instanceRepo != "myorg/inst" {
		t.Errorf("local caller = %+v", c)
	}
}
