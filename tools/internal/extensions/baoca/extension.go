package baoca

// extension.go — `openbao-peer-ca` declares itself: the CA slice of the
// `openbao-lifecycle` row.
//
// FORTY-FIFTH EXTENSION, AND THE SECOND SLICE OF ONE CATALOG ROW. `openbao-seed`
// took the seeding third last time and recorded why the row cannot move whole: 13
// files measuring 20 outbound against ~25 inbound across six other files is not
// one boundary. This is the next boundary that actually exists — two verbs whose
// entire inbound surface is their own cobra constructors.
//
// It matters that these two are ONE extension and not two. `extract-openbao-ca`
// reads the standby's CA and emits it as a step output; `provision-peer-ca` writes
// the peer's CA into openbao-peer-tls. They are the two halves of one exchange
// across an HA pair, and an instance that does one without the other has a cluster
// that trusts nobody. Same reasoning that kept `database-provisioner` and
// `assert-database` together, and the opposite of `credential-pat`/`objkey`.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `openbao-peer-ca` declaration.
//
//	transition:provisioned[cluster-read, cluster-write]
//
// `provisioned` RATHER THAN `seeded`, and the distinction is the one this model
// keeps rewarding. What crosses here is a CA CERTIFICATE — public material that
// lets two OpenBao halves authenticate each other. It is not a credential: nothing
// here mints, holds or places a secret, so `secret-custody` would be a false
// claim, and `seeded` ("credentials and secret material are in place") would be
// the wrong moment. Peer trust is substrate — it belongs with the cluster coming
// up, before anything has a credential to seed.
//
// `cluster-write` because provisioning applies a Secret; `cluster-read` because
// extraction reads one. Both are cluster operations end to end: this extension
// never speaks to OpenBao's API at all, which is why it is separable from the rest
// of its row and the rest of its row is not.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "openbao-peer-ca",
		Short:  "exchange the CA certificates the two OpenBao halves authenticate each other with",
		Always: false,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Provisioned,
			Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
		}},
		Incomplete: []string{
			"this is the CA slice of the catalog's openbao-lifecycle row, and the second " +
				"slice taken (openbao-seed was the first). Eight files remain in package main: " +
				"init, ensure-ready/unseal, breakglass, regen-root, configure and the login " +
				"paths. They are entangled with ci_harbor, ci_health_sla, ci_converge and " +
				"verify; take the next boundary that exists rather than the row.",
		},
	}
}
