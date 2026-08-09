package reconcilelanes

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("reconcile-actions does not validate: %v", err)
		}
	}
}

// THE OVER-GRANTING ARGUMENT, as a test rather than a comment.
//
// The catalog's claim is that collapsing these lanes into one binding widens the
// grants to their union. Assert the consequence directly: no single binding may
// hold both cluster-write and secret-custody, because no lane needs both. If a
// future edit merges them, the union appears on one binding and this fails.
func TestNoLaneHoldsAnotherLanesCapability(t *testing.T) {
	for _, b := range Extension().Bindings {
		var write, custody bool
		for _, g := range b.Grants {
			switch g {
			case extension.ClusterWrite:
				write = true
			case extension.SecretCustody:
				custody = true
			}
		}
		if write && custody {
			t.Errorf("%s holds BOTH cluster-write and secret-custody — that pair is the union of "+
				"two different lanes, not any one lane's need. The read-only OpenBao sampler would "+
				"gain permission to patch StorageClasses, and the cluster lanes would gain an "+
				"OpenBao token.", b)
		}
	}
}

// Four named invariants at one state. Without Binding.Name the model caps an
// extension at ONE invariant, since `operating` is the only state they may attach
// to — so the names are what make this extension expressible at all.
func TestFourNamedInvariantsAreDistinct(t *testing.T) {
	e := Extension()
	seen := map[string]bool{}
	for _, b := range e.Bindings {
		if b.Kind != extension.Invariant {
			t.Errorf("%s is not an invariant; every lane here is a property that must keep holding", b)
			continue
		}
		if b.State != extension.Operating {
			t.Errorf("%s is not at operating", b)
		}
		if b.Name == "" {
			t.Error("an unnamed invariant collides with its siblings")
		}
		if seen[b.Name] {
			t.Errorf("duplicate lane name %q", b.Name)
		}
		seen[b.Name] = true
	}
	if len(seen) != 4 {
		t.Fatalf("want four named lanes, got %d: %v", len(seen), seen)
	}

	// And the names are load-bearing, not decorative: strip them and the model
	// must refuse the extension outright.
	e = Extension()
	for i := range e.Bindings {
		e.Bindings[i].Name = ""
	}
	if errs := e.Validate(); len(errs) == 0 {
		t.Error("four unnamed invariants at the same state must be refused as duplicates")
	}
}

// secret-custody at `operating` is legal and needed no ceiling change — the
// control case for the cloud-mutate row assert-storage had to add. Pin it: if the
// grantStates row for secret-custody is ever narrowed to {seeded}, the OpenBao
// gauges become inexpressible and this says so here, beside the lane.
func TestTheOpenBaoSamplerCanDeclareItsCustody(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Name != "openbao-gauges" {
			continue
		}
		one := extension.Extension{Name: "probe", Short: "x", Bindings: []extension.Binding{b}}
		if errs := one.Validate(); len(errs) > 0 {
			t.Errorf("openbao-gauges cannot be declared: %v — it authenticates through the "+
				"reconciler's k8s-auth role and reads credential metadata continuously", errs)
		}
		return
	}
	t.Fatal("openbao-gauges binding is missing")
}
