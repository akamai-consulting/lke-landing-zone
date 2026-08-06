package healthsla

// extension.go — `health-sla` declares itself.
//
// TWELFTH EXTENSION. Four scheduled checks that run against a cluster which is
// already working, forever: two rotation SLAs (is this credential overdue?) and
// two readiness reports (is this component healthy right now?).
//
// WHAT THE CATALOG GOT WRONG. It grouped three files by FILENAME PREFIX —
// ci_health_sla.go, ci_health_readiness.go, ci_health_incluster.go — and called
// them one extension. The third does not belong: `health-incluster` computes the
// CONVERGENCE VERDICT over internal/kube with the pod ServiceAccount, sharing the
// exit-code contract and the convergence classifier with ci_health.go. It is part
// of `converge`, and it stayed behind for that extraction. Grouping by name
// prefix is grouping by how someone once filed the code, which is exactly the
// misfiling ADR 0014 says package main is full of.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `health-sla` declaration.
//
// TWO NAMED INVARIANTS, because their capabilities differ:
//
//	invariant:operating "rotation-sla"        [cluster-read, secret-custody]
//	invariant:operating "component-readiness" [cluster-read]
//
// WHY SPLIT. `guard-charts` established that a split must be justified by
// DIVERGENT CAPABILITY rather than by count, and this is the case that divergence
// actually arises: the Loki OBJ-key SLA reads OPENBAO_ROOT_TOKEN out of the
// environment and execs `bao kv metadata get` with it. The readiness checks never
// hold a credential. Collapsing these into one binding would widen its grants to
// the union and hand the readiness lane secret-custody it does not use, which is
// the over-granting that per-binding grants exist to prevent.
//
// WHY `invariant` AND NOT `assertion`. Both kinds observe. An assertion answers
// "did the thing I just did work?" and is bounded by that transition; these are
// scheduled, run forever on a cluster nobody is currently changing, and their
// failure means the platform DRIFTED rather than that a step failed. `operating`
// is the only state an invariant may attach to, which is the right answer here.
//
// WHY secret-custody FOR A CHECK THAT ONLY READS A TIMESTAMP. This is the honest
// reading and it is worth stating, because the narrower one is tempting: the code
// calls `kv metadata get` and uses only `updated_time`, never the secret material.
// But the model judges what a binding IS HANDED, not what it promises to do with
// it — the grant is "the handle the action receives", and this action receives the
// OpenBao ROOT token, which opens every secret in the store. A reviewer reading
// `[cluster-read]` on this lane would not learn that. See the catalog for the gap
// this exposes: the vocabulary cannot yet distinguish "handed root, reads
// metadata" from "handed root, reads material".
//
// No ceiling change. secret-custody became legal at `operating` before this
// existed, and `cluster-read` is unrestricted.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "health-sla",
		Short:  "scheduled rotation-SLA and component-readiness checks against a running cluster",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Invariant,
				Name:   "rotation-sla",
				State:  extension.Operating,
				Grants: []extension.Grant{extension.ClusterRead, extension.SecretCustody},
			},
			{
				Kind:   extension.Invariant,
				Name:   "component-readiness",
				State:  extension.Operating,
				Grants: []extension.Grant{extension.ClusterRead},
			},
		},
	}
}
