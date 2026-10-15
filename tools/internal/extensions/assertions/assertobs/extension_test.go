package assertobs

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-observability does not validate: %v", err)
		}
	}
}

// EVERY LANE OBSERVES. The extension once carried a cluster-writing transition
// (the obj-proxy CA retrofit); with it gone, a mutating grant here would be a
// write nothing declares a reason for.
func TestObservingLanesStayReadOnly(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Assertion {
			t.Errorf("%s: kind = %s, want assertion", b.Name, b.Kind)
		}
		for _, g := range b.Grants {
			if g == extension.ClusterWrite || g == extension.CloudMutate || g == extension.SecretCustody {
				t.Errorf("%s holds %q — every assert-observability lane only reads", b.Name, g)
			}
		}
	}
}
