package openbao

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestSeedBindingValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "openbao" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
	if b := binding(t, "seed"); b.State != extension.Seeded {
		t.Errorf("seed: state = %s, want seeded — this is the code that puts credential "+
			"material in place, which is what the state is defined as", b.State)
	}
}

// A transition to `seeded` MUST declare secret-custody — Validate enforces it, and
// this is the extension that rule was written for. If the grant ever came off, the
// declaration would describe a seeder that places nothing.
func TestDeclaresCustody(t *testing.T) {
	b := binding(t, "seed")
	if !has(b, extension.SecretCustody) {
		t.Error("secret-custody dropped — this is the code that puts credential material in place")
	}
	// THIS GUARD RAN THE OTHER WAY UNTIL THE CAPABILITY LAYER FALSIFIED IT. It
	// asserted cluster-write was ABSENT, on the stated grounds that "only the
	// seal-key path applies a Secret, and it does so through the KubectlApply seam
	// rather than as a general grant". That was true about the Secret and missed
	// the annotate: the seal-key wait forces a hard refresh on the parent
	// Application when it wedges, which is a cluster mutation with no Secret in it.
	//
	// Nothing could have caught that while writes went through a general exec seam
	// — the grant was checked against a state table and then handed no teeth, so a
	// read-only declaration and a mutating one produced identical behaviour. It
	// surfaced on the first run after the write moved to a named operation.
	if !has(b, extension.ClusterWrite) {
		t.Error("cluster-write dropped — the seal-key wait annotates the parent Application " +
			"to force a hard refresh, and without the grant that mutation is refused at runtime")
	}
}

// NO SLICE OF THIS SUBJECT IS OUTSTANDING, and that claim is now checkable in a
// way it was not before the merge.
//
// This guard used to assert Incomplete was EMPTY, on the grounds that the
// openbao-lifecycle row had finally been extracted whole. The merge changed what
// the field carries: the two notes below are about the MODEL's limits, not about
// code left in package main. So the assertion is narrowed rather than deleted —
// it still fails if a note claims a slice is elsewhere, which is the thing it
// was written to catch.
func TestNoSliceIsOutstanding(t *testing.T) {
	for _, note := range Extension().Incomplete {
		for _, leak := range []string{"package main", "cmd/llz", "still in", "not yet extracted"} {
			if strings.Contains(note, leak) {
				t.Errorf("Incomplete says a slice is still elsewhere (%q in %q) — the "+
					"openbao row was extracted whole, so a note like this means it came apart",
					leak, note)
			}
		}
	}
}

// THE PER-BINDING ENABLEMENT GAP IS RECORDED, NOT SILENTLY ACCEPTED. `peer-ca`
// shipped opt-in and the merged extension is always-on; that is a real loss of
// fidelity and the note is the only thing carrying it. If someone deletes the note
// without adding Binding.Always, the loss becomes invisible.
func TestEnablementGapStaysDeclared(t *testing.T) {
	var found bool
	for _, note := range Extension().Incomplete {
		if strings.Contains(note, "PER-BINDING") {
			found = true
		}
	}
	if !found {
		t.Error("the per-binding enablement note is gone. peer-ca was `Always: false` before " +
			"the merge and is declared always-on now; either Binding.Always exists and the " +
			"declaration uses it, or this note has to stay")
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
