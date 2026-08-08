package openbao

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// binding returns the named binding, failing loudly if it is gone. Every
// assertion in this file and seed_extension_test.go goes through it, because the
// merge that folded four extensions into one turned "the extension's grants" into
// a UNION that says nothing about any single lane. Asserting on e.HasGrant here
// would now pass for the wrong reason: `secret-custody` is legitimately held by
// four of the five bindings, so a whole-extension check could never catch
// `peer-ca` acquiring it.
func binding(t *testing.T, name string) extension.Binding {
	t.Helper()
	for _, b := range Extension().Bindings {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("binding %q is gone — it was one of the five this extension merged, and its "+
		"absence means a capability stopped being declared rather than stopped existing", name)
	return extension.Binding{}
}

func has(b extension.Binding, want extension.Grant) bool {
	for _, g := range b.Grants {
		if g == want {
			return true
		}
	}
	return false
}

func TestPeerCABindingValidates(t *testing.T) {
	e := Extension()
	if errs := e.Validate(); len(errs) != 0 {
		t.Fatalf("declaration does not validate: %v", errs)
	}
	if e.Name != "openbao" {
		t.Errorf("identity drifted: %q", e.Name)
	}
	if b := binding(t, "peer-ca"); b.State != extension.Provisioned {
		t.Errorf("peer-ca: state = %s, want provisioned — peer trust is substrate, and it "+
			"belongs with the cluster coming up rather than with anything being seeded", b.State)
	}
}

// A CA CERTIFICATE IS NOT A CREDENTIAL. Nothing in this lane mints, holds or
// places secret material — it exchanges public keys so two halves can
// authenticate. If secret-custody ever appears ON THIS BINDING, either it grew a
// credential path or the declaration started overclaiming, and both deserve a
// look.
//
// This is also the control case for the merge. Four sibling bindings hold custody
// legitimately; this one must not. A grant judged per binding rather than
// pattern-matched onto the package is the whole reason the merge stopped at the
// package boundary.
func TestPeerCAClaimsNoCustody(t *testing.T) {
	b := binding(t, "peer-ca")
	if has(b, extension.SecretCustody) {
		t.Error("peer-ca declares secret-custody — it exchanges CA certificates, which are public")
	}
	if has(b, extension.SecretRead) {
		t.Error("peer-ca declares secret-read — the ca.crt it reads is the public half")
	}
	for _, g := range []extension.Grant{extension.ClusterRead, extension.ClusterWrite} {
		if !has(b, g) {
			t.Errorf("peer-ca dropped %q — extraction reads a Secret and provisioning applies one", g)
		}
	}
}

// It never speaks to a cloud API, which is exactly why it was separable from the
// rest of the lifecycle row. If that changes, this stops being a boundary.
func TestPeerCAStaysAClusterOnlyExchange(t *testing.T) {
	b := binding(t, "peer-ca")
	if has(b, extension.CloudRead) || has(b, extension.CloudMutate) {
		t.Error("peer-ca acquired a cloud grant — this is a cluster-only exchange. Its siblings " +
			"`init` and `regen-root` hold cloud-mutate, so a whole-extension check would " +
			"never have caught this")
	}
}
