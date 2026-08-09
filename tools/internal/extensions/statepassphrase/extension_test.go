package statepassphrase

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
	if e.Name != "credential-state-passphrase" || !e.Always {
		t.Errorf("identity drifted: name=%q always=%v", e.Name, e.Always)
	}
}

// THE REFUSAL THIS DECLARATION IS BUILT ON. If secret-custody ever becomes legal
// at `configured`, this fails — and the right response is to check whether the row
// was widened for a credential that HAS an issuer, or whether the guard against
// hardcoded secrets was quietly dropped to accommodate this one.
func TestCustodyAtConfiguredStaysRefused(t *testing.T) {
	probe := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.SecretCustody},
		}},
	}
	if errs := probe.Validate(); len(errs) == 0 {
		t.Error("secret-custody became legal at `configured` — this extension's binding was " +
			"pushed to `seeded` precisely because it is not. Re-read the grantStates comment: " +
			"custody before anything can ISSUE a credential is the shape of a hardcoded secret")
	}
}

// The push is recorded, not hidden. Dropping the note leaves a binding at `seeded`
// for something that runs at `configured`, with nothing saying so.
func TestForcedStateStaysMarked(t *testing.T) {
	inc := strings.ToLower(strings.Join(Extension().Incomplete, " "))
	if inc == "" {
		t.Fatal("Incomplete was emptied — this binding is at the nearest LEGAL state, not the true one")
	}
	if !strings.Contains(inc, "configured") {
		t.Error("the note no longer names the state this actually runs at")
	}
}

// It mints and stores a passphrase, and writes a GitHub Environment secret.
func TestDeclaresCustodyAndTheForgeWrite(t *testing.T) {
	e := Extension()
	for _, g := range []extension.Grant{extension.SecretCustody, extension.CloudMutate} {
		if !e.HasGrant(g) {
			t.Errorf("%q dropped — this mints a passphrase and writes it to a GitHub Environment", g)
		}
	}
}
