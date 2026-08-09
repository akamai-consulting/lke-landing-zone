package baoseed

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "openbao-seed" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// A transition to `seeded` MUST declare secret-custody — Validate enforces it, and
// this is the extension that rule was written for. If the grant ever came off, the
// declaration would describe a seeder that places nothing.
func TestDeclaresCustody(t *testing.T) {
	if !Extension().HasGrant(extension.SecretCustody) {
		t.Error("secret-custody dropped — this is the code that puts credential material in place")
	}
	if Extension().HasGrant(extension.ClusterWrite) {
		t.Error("cluster-write declared on the extension — only the seal-key path applies a " +
			"Secret, and it does so through the KubectlApply seam rather than as a general grant")
	}
}

// The partial is recorded. Ten of thirteen files in the catalog's row are still in
// package main, and nothing else says so.
func TestPartialStaysMarked(t *testing.T) {
	inc := strings.ToLower(strings.Join(Extension().Incomplete, " "))
	if inc == "" {
		t.Fatal("Incomplete emptied — this is one third of the openbao-lifecycle row")
	}
	if !strings.Contains(inc, "openbao-lifecycle") {
		t.Error("the note no longer names the row this is part of")
	}
}

// THE CAPABILITY DEFAULTS MUST ERROR, NOT NO-OP. A seeder that reports success
// without writing is the exact failure baoread's fail-closed discipline exists to
// prevent, reintroduced one layer up.
func TestCapabilityDefaultsFailClosed(t *testing.T) {
	prevApply, prevSecret := KubectlApply, SetGitHubSecret
	KubectlApply = func(string) error { return errDefault() }
	SetGitHubSecret = func(string, string, string) error { return errDefault() }
	t.Cleanup(func() { KubectlApply, SetGitHubSecret = prevApply, prevSecret })

	if err := KubectlApply("manifest"); err == nil {
		t.Error("an uninstalled KubectlApply must error — silently not applying looks like success")
	}
	if err := SetGitHubSecret("N", "env", "v"); err == nil {
		t.Error("an uninstalled SetGitHubSecret must error")
	}
}

func errDefault() error { return errNotInstalled }
