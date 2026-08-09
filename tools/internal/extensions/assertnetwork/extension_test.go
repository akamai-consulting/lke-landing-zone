package assertnetwork

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-network does not validate: %v", err)
		}
	}
}

// EVERY LANE HERE OBSERVES. That is not obvious from the outside — asserting a
// NetworkPolicy denies a connection sounds like it needs something to attempt the
// connection from — so pin it. The pod is created by the workflow; net-probe is
// the dial that runs inside it.
//
// If a mutating grant ever appears, one of two things happened: a lane started
// creating its own probe fixture, or a lane started repairing what it found.
// Both are transitions and belong in a separate binding, at a state where
// transitions are legal — `verified` is not one.
func TestNoLaneMutates(t *testing.T) {
	mutating := map[extension.Grant]bool{
		extension.ClusterWrite:  true,
		extension.CloudMutate:   true,
		extension.SecretCustody: true,
		extension.OwnPaths:      true,
	}
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Assertion {
			t.Errorf("%s: kind = %s, want assertion", b.Name, b.Kind)
		}
		if b.State != extension.Verified {
			t.Errorf("%s: state = %s, want verified — these attest a property of a running "+
				"platform rather than moving it anywhere", b.Name, b.State)
		}
		for _, g := range b.Grants {
			if mutating[g] {
				t.Errorf("%s holds %q. If a lane now needs to create its probe fixture, that "+
					"half is a TRANSITION — and note `verified` does not accept one, so it "+
					"needs a state as well as a binding", b.Name, g)
			}
		}
	}
}

// net-probe reaches nothing: its body is a net.DialTimeout and an exit code. The
// grant line should say so, and a cluster grant appearing here would mean the lane
// has stopped being the thing that runs inside the pod.
func TestNetProbeReachesNothing(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Name != "net-probe" {
			continue
		}
		if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
			t.Errorf("grants = %v, want [read-repo] only — this runs INSIDE the probe pod and "+
				"dials a TCP address; it does not query the cluster", b.Grants)
		}
		return
	}
	t.Fatal("no binding named \"net-probe\"")
}
