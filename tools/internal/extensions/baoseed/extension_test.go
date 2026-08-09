package baoseed

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
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

// THE ROW IS WHOLE NOW, so this guard runs the other way. It used to assert the
// note stayed non-empty and kept naming openbao-lifecycle, because ten of that
// row's files were still in package main. They were extracted one boundary at a
// time — baolifecycle, identityconfig, openbao, baoca, harbor, healthsla — so the
// note was deleted. A note reappearing here means a slice left again, and that
// should be argued rather than typed.
func TestNoSliceIsOutstanding(t *testing.T) {
	if inc := strings.Join(Extension().Incomplete, " "); inc != "" {
		t.Errorf("Incomplete came back (%q) — name the slice that left and why", inc)
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
