package assertsecrets

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("assert-secrets does not validate: %v", err)
		}
	}
}

// The drill applies and deletes a Job — you cannot assert a rotation ROTATES
// without running one. Four extractions in a row have found a mutation hiding
// behind an `assert-` prefix, so this pins the one here.
func TestTheDrillsWriteStaysDeclared(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Name != "broad-pat-drill" {
			continue
		}
		if b.Kind != extension.Transition {
			t.Errorf("kind = %s, want transition — it kubectl-applies a Job", b.Kind)
		}
		var writes bool
		for _, g := range b.Grants {
			if g == extension.ClusterWrite {
				writes = true
			}
		}
		if !writes {
			t.Error("dropped cluster-write — the drill still applies and deletes a Job")
		}
		return
	}
	t.Fatal("no binding named \"broad-pat-drill\"")
}

// The other three observe, and must not inherit the drill's grant.
func TestObservingLanesStayReadOnly(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Name == "broad-pat-drill" {
			continue
		}
		if b.Kind != extension.Assertion {
			t.Errorf("%s: kind = %s, want assertion", b.Name, b.Kind)
		}
		for _, g := range b.Grants {
			switch g {
			case extension.ClusterRead, extension.SecretRead, extension.CloudRead, extension.ReadRepo:
			default:
				t.Errorf("%s holds %q — these lanes JUDGE credentials, they do not place or "+
					"change them. secret-custody in particular would claim this writes credential "+
					"material", b.Name, g)
			}
		}
	}
}

// rotation-health is the only lane at `operating`: it reads
// llz_credential_age_days continuously and fails when a credential drifts past its
// SLA. The others attest a property once, after a change.
func TestOnlyRotationHealthIsContinuous(t *testing.T) {
	for _, b := range Extension().Bindings {
		want := extension.Verified
		if b.Name == "rotation-health" {
			want = extension.Operating
		} else if b.Name == "broad-pat-drill" {
			want = extension.Converged
		}
		if b.State != want {
			t.Errorf("%s: state = %s, want %s", b.Name, b.State, want)
		}
	}
}
