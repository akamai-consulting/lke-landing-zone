package healthsla

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("health-sla does not validate: %v", err)
		}
	}
}

// The split is the declaration's whole content, so pin it. Two invariants at the
// same state are only legal because they are NAMED, and they are only worth
// naming because their grants differ — collapse them and the readiness lane
// silently gains secret-custody it never uses.
func TestHealthSLASplitsOnCapabilityNotCount(t *testing.T) {
	byName := map[string][]extension.Grant{}
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Invariant {
			t.Errorf("%s: kind = %s, want invariant — these are scheduled checks on a "+
				"cluster nobody is changing; failure means DRIFT, not a failed step", b.Name, b.Kind)
		}
		if b.State != extension.Operating {
			t.Errorf("%s: state = %s, want operating — the only state an invariant may hold", b.Name, b.State)
		}
		byName[b.Name] = b.Grants
	}
	if len(byName) != 2 {
		t.Fatalf("want exactly two named bindings, got %v", byName)
	}

	has := func(name string, g extension.Grant) bool {
		for _, got := range byName[name] {
			if got == g {
				return true
			}
		}
		return false
	}
	if !has("rotation-sla", extension.SecretCustody) {
		t.Error("rotation-sla dropped secret-custody — the Loki OBJ-key check still reads " +
			"OPENBAO_ROOT_TOKEN and execs bao with it, so dropping the grant makes the " +
			"DECLARATION the thing that is wrong")
	}
	if has("component-readiness", extension.SecretCustody) {
		t.Error("component-readiness gained secret-custody — it holds no credential. This is " +
			"exactly the over-granting that splitting on capability was meant to prevent; if " +
			"the readiness checks now need a credential, that is a change worth arguing for")
	}
	for _, name := range []string{"rotation-sla", "component-readiness"} {
		if !has(name, extension.ClusterRead) {
			t.Errorf("%s dropped cluster-read — every check here reads the cluster", name)
		}
	}
}
