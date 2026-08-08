package gameday

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "wedge-gameday" {
		t.Errorf("identity drifted: %q", e.Name)
	}
}

// Deliberate fault injection against a healthy cluster must not ship enabled.
// Always is a default rather than a constant, so an instance can still turn it on —
// but the default is the safe direction.
func TestStaysOptIn(t *testing.T) {
	if Extension().Always {
		t.Error("wedge-gameday became always-on — it BREAKS a platform ExternalSecret on purpose; " +
			"that is a thing an operator chooses, not a thing an instance inherits")
	}
}

// It patches an ExternalSecret. Declaring only cluster-read would be a lie, and the
// restore path does not make it read-only: a mode that cleans up after itself still
// wrote.
func TestDeclaresTheFaultItInjects(t *testing.T) {
	e := Extension()
	if !e.HasGrant(extension.ClusterWrite) {
		t.Error("cluster-write dropped — this repoints a secretStoreRef to inject the fault")
	}
	if !e.Binds(extension.Transition) {
		t.Error("a binding that mutates cannot be an assertion: assertions hold read grants only")
	}
}

// THE STATE IS A FORCED MOVE, and this test records why rather than asserting the
// forcing is correct. If bindableStates ever lets a Transition reach `operating`,
// this fails and the declaration should be revisited — a gameday REQUIRES a steady
// platform (it refuses to start unless the cluster is Healthy) rather than
// establishing one.
func TestStateIsTheNearestLegalOne(t *testing.T) {
	b := Extension().Bindings[0]
	if b.State != extension.Converged {
		t.Fatalf("state = %q, want %q", b.State, extension.Converged)
	}
	for _, s := range bindable(extension.Transition) {
		if s == extension.Operating {
			t.Error("Transition can now reach `operating` — wedge-gameday belongs there: " +
				"it requires a steady platform rather than establishing one")
		}
	}
}

func bindable(k extension.BindingKind) []extension.State {
	var out []extension.State
	for _, s := range extension.States() {
		probe := extension.Extension{
			Name: "probe", Short: "x",
			Bindings: []extension.Binding{{Kind: k, State: s}},
		}
		if len(probe.Validate()) == 0 {
			out = append(out, s)
		}
	}
	return out
}
