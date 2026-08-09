package baoseed

// extension.go — `openbao-seed` declares itself: the seeding third of the
// `openbao-lifecycle` row.
//
// FORTY-FOURTH EXTENSION, AND A DELIBERATE PARTIAL. The catalog's
// `openbao-lifecycle` row is 13 files and ~2,185 lines — the largest single unit
// left — and measuring the whole set reported an outbound closure of 20 against
// ~25 inbound symbols spread across ci_harbor, ci_health_sla, ci_converge,
// secret_apply, verify and ci_keycloak_configure. That is not one boundary; it is
// six.
//
// The seeders are a boundary. `ci_bao_seed.go`, `ci_bao_seed_all.go` and
// `ci_bao_seed_seal_key.go` measure 11 outbound between them, of which two were
// genuine capabilities and the rest were printers, flags, or symbols this campaign
// had ALREADY turned into seams — `baoKVPutFn` is `baoread.KVPut`, `openbaoNS` is
// `baoread.Namespace`. The four earlier credential walls paid for this one.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `openbao-seed` declaration.
//
//	transition:seeded[read-repo, cluster-read, secret-custody]
//
// THE STATE IS EXACTLY RIGHT FOR ONCE, which is worth noting after three
// extractions running whose bindings had to be pushed. `seeded` is defined as
// "credentials and secret material are in place", and this is the code that puts
// them there — the `Validate()` rule that a transition to `seeded` MUST declare
// `secret-custody` was written for precisely this shape, and it passes without
// argument.
//
// `cluster-read` because a field source may be `k8s:NS/SECRET/KEY` — a value the
// platform already holds, copied into OpenBao. `read-repo` for the tfvars and spec
// the seal-key path consults. No `cluster-write` on the main seeder: the seal key
// applies a Secret, and that is the one path holding KubectlApply.
//
// WHAT IT REFUSES TO DO IS THE POINT. A seed that cannot READ a path does not
// overwrite it: baoread's fail-closed verdict distinguishes "bao answered: absent"
// from "nothing answered", and only the first permits a write. The failure it
// prevents is a seeder that treats an unreachable OpenBao as an empty one and
// overwrites a live credential — which is why this package's two capability
// defaults ERROR rather than doing nothing.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "openbao-seed",
		Short:  "place credential material into OpenBao, and never overwrite a path it could not read",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Seeded,
			Grants: []extension.Grant{extension.ReadRepo, extension.ClusterRead, extension.SecretCustody},
		}},
		Incomplete: []string{
			"this is the SEEDING third of the catalog's openbao-lifecycle row. The other " +
				"ten files — init, unseal/ensure-ready, breakglass, regen-root, the peer CA, " +
				"configure and the login paths — are still in package main. They are entangled " +
				"with ci_harbor, ci_health_sla, ci_converge, secret_apply, verify and " +
				"ci_keycloak_configure; the set measures 20 outbound against ~25 inbound, which " +
				"is six boundaries rather than one.",
		},
	}
}
