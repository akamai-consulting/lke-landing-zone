package baoca

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
	if e.Name != "openbao-peer-ca" {
		t.Errorf("identity drifted: %q", e.Name)
	}
}

// A CA CERTIFICATE IS NOT A CREDENTIAL. Nothing here mints, holds or places
// secret material — it exchanges public keys so two halves can authenticate. If
// secret-custody ever appears, either this grew a credential path or the
// declaration started overclaiming, and both deserve a look.
func TestClaimsNoCustody(t *testing.T) {
	e := Extension()
	if e.HasGrant(extension.SecretCustody) {
		t.Error("secret-custody declared — this exchanges CA certificates, which are public")
	}
	if e.HasGrant(extension.SecretRead) {
		t.Error("secret-read declared — the ca.crt this reads is the public half")
	}
	for _, g := range []extension.Grant{extension.ClusterRead, extension.ClusterWrite} {
		if !e.HasGrant(g) {
			t.Errorf("%q dropped — extraction reads a Secret and provisioning applies one", g)
		}
	}
}

// It never speaks to OpenBao's API, which is exactly why it was separable from
// the rest of its row. If that changes, this stops being a boundary.
func TestStaysAClusterOnlyExchange(t *testing.T) {
	if Extension().HasGrant(extension.CloudRead) || Extension().HasGrant(extension.CloudMutate) {
		t.Error("a cloud grant appeared — this is a cluster-only exchange, and that is what " +
			"made it separable from openbao-lifecycle's other eight files")
	}
	inc := strings.ToLower(strings.Join(Extension().Incomplete, " "))
	if !strings.Contains(inc, "openbao-lifecycle") {
		t.Error("the note no longer names the row this is a slice of")
	}
}
