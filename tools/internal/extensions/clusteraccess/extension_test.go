package clusteraccess

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func TestExtensionDeclarationValidates(t *testing.T) {
	if errs := Extension().Validate(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("cluster-access does not validate: %v", err)
		}
	}
}

// `teardown` was the first transition; this is the first at the START of the
// lifecycle. Assert the binding claims to CHANGE the world rather than report on
// it.
//
// The state matters as much as the kind. `provisioned` is the only place this can
// sit — earlier there is no control plane to open and no kubeconfig to fetch, and
// later is a lie, because `seeded` and everything after it PRESUME both already
// happened. A binding that drifted to `seeded` would still validate today, which is
// why it is pinned here rather than left to Validate().
func TestClusterAccessIsATransitionAtProvisioned(t *testing.T) {
	e := Extension()
	if len(e.Bindings) != 1 {
		t.Fatalf("want one binding — fetch and ACL are one moment, not two; got %v", e.Bindings)
	}
	b := e.Bindings[0]
	if b.Kind != extension.Transition {
		t.Errorf("kind = %s, want transition — this one moves the world; after it an API "+
			"server that refused this runner answers it", b.Kind)
	}
	if b.State != extension.Provisioned {
		t.Errorf("state = %s, want provisioned — every later state presumes cluster access "+
			"and none of them perform it", b.State)
	}
}

// Over-granting is the failure mode a transition has and a gate does not, so spell
// out the whole set rather than counting it. Each of these is load-bearing:
// dropping cloud-mutate would misdescribe an ACL PUT that races every other runner;
// dropping cluster-write would hide the lease ConfigMap that makes that race
// survivable; dropping secret-custody would hide a cluster-admin kubeconfig landing
// on disk, which is the one credential per cluster a human can actually use.
func TestClusterAccessGrantsAreExactlyTheThreeItHolds(t *testing.T) {
	want := map[extension.Grant]bool{
		extension.CloudMutate:   true,
		extension.ClusterWrite:  true,
		extension.SecretCustody: true,
	}
	got := map[extension.Grant]bool{}
	for _, g := range Extension().Bindings[0].Grants {
		got[g] = true
	}
	for g := range want {
		if !got[g] {
			t.Errorf("missing grant %q — the code still does this, so dropping it from the "+
				"declaration makes the declaration the thing that is wrong", g)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("unexpected grant %q — a transition that asks for more than it uses is "+
				"the over-granting the model exists to make visible", g)
		}
	}
}

// secret-custody at `provisioned` is the row this extension added to grantStates,
// and it is legal in exactly one direction: the credential is FETCHED from the
// cloud, not invented. Assert the declaration still exercises that row, so that if
// someone narrows grantStates back, this fails with the reason attached rather than
// leaving a widened table nothing depends on.
func TestSecretCustodyAtProvisionedRemainsExercised(t *testing.T) {
	b := Extension().Bindings[0]
	for _, g := range b.Grants {
		if g == extension.SecretCustody {
			if errs := Extension().Validate(); len(errs) > 0 {
				t.Fatalf("secret-custody at %s no longer validates — grantStates was narrowed, "+
					"but RunFetch still writes a cluster-admin kubeconfig to disk: %v", b.State, errs)
			}
			return
		}
	}
	t.Fatal("declaration no longer holds secret-custody; if RunFetch stopped writing a " +
		"kubeconfig, narrow grantStates too rather than leaving an unused row")
}
