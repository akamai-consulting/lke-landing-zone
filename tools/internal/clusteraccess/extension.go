package clusteraccess

// extension.go — `cluster-access` declares itself.
//
// ELEVENTH EXTENSION. `teardown` was the first transition; this is the first one
// at the START of the lifecycle rather than its end, and that difference is the
// whole finding — see the grants note below.
//
// WHAT IT IS. Between "the cluster exists" and "anything can be done to it" sits
// a step nothing else can skip: get an admin kubeconfig out of Linode, and put
// this runner's egress IP on the control-plane ACL so the API server will answer
// it. Every later state — seeded, converged, operating — presumes both, and none
// of them perform either.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `cluster-access` declaration.
//
// `transition:provisioned[cloud-mutate, cluster-write, secret-custody]`.
//
// TRANSITION, NOT GATE. Most of the ten before it observed and reported; a second
// run changed nothing. This one moves the world: after it, an API server that
// refused this runner answers it.
//
// WHY EACH GRANT, since a transition is where over-granting would first do damage:
//
//   - cloud-mutate — RunACL PUTs the LKE control-plane ACL. Last-writer-wins
//     against every other runner (acl.go documents the race), which is precisely
//     why it is declared rather than assumed.
//   - cluster-write — the ACL lease is a ConfigMap. A concurrent job's kubectl
//     must be able to see that this runner holds the ACL, and in-cluster state is
//     the only place both jobs can look.
//   - secret-custody — RunFetch writes a cluster-admin kubeconfig to disk. It is
//     the single human-facing credential per cluster (docs/designs/team-scoped-
//     credentials.md), so it is custody in the strictest sense the model has.
//
// THE MODEL DID NOT ALLOW THE THIRD ONE. grantStates listed secret-custody as
// legal at `seeded` and `operating` only, and Validate() rejected this
// declaration. The table was not wrong so much as it had only ever been shown
// credentials that the platform MINTS (seeding) or REPLACES (rotation) — both of
// which happen to a cluster that already works. It had never been shown the
// bootstrap credential: the one the CLOUD issues at provisioning time, which is
// the reason a cluster can be seeded at all. See internal/extension/validate.go
// for the row and the argument; this is the second grantStates widening, and like
// the first (cloud-mutate at `operating`) it was made against shipping code that
// could not otherwise be described.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "cluster-access",
		Short:  "fetch the admin kubeconfig and hold the control-plane ACL open for this runner",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			State: extension.Provisioned,
			Grants: []extension.Grant{
				extension.CloudMutate,
				extension.ClusterWrite,
				extension.SecretCustody,
			},
		}},
	}
}
