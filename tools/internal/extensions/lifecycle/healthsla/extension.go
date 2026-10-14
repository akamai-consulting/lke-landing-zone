package healthsla

// extension.go — `health-sla` declares itself.
//
// TWELFTH EXTENSION. Scheduled checks that run against a cluster which is already
// working, forever: one rotation SLA (is this credential overdue?) and two
// readiness reports (is this component healthy right now?). It was two rotation
// SLAs until #483 retired the Loki OBJ-key one, whose measurement needed a root
// token bootstrap revokes — see sla.go's header.
//
// WHAT THE CATALOG GOT WRONG. It grouped three files by FILENAME PREFIX —
// ci_health_sla.go, ci_health_readiness.go, ci_health_incluster.go — and called
// them one extension. The third does not belong: `health-incluster` computes the
// CONVERGENCE VERDICT over internal/kube with the pod ServiceAccount, sharing the
// exit-code contract and the convergence classifier with ci_health.go. It is part
// of `converge`, and it stayed behind for that extraction. Grouping by name
// prefix is grouping by how someone once filed the code, which is exactly the
// misfiling ADR 0014 says package main is full of.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `health-sla` declaration.
//
// TWO NAMED INVARIANTS, because their capabilities differ:
//
//	invariant:operating "rotation-sla"        [cluster-read, secret-read]
//	invariant:operating "component-readiness" [cluster-read]
//
// WHY SPLIT. `guard-charts` established that a split must be justified by
// DIVERGENT CAPABILITY rather than by count, and the divergence is real here: the
// rotation SLA LISTS SECRETS (kube-system lke-admin-token, for their
// creationTimestamps), and the readiness checks read no credential object at all.
// Collapsing these into one binding would widen its grants to the union and hand
// the readiness lane secret-read it does not use, which is the over-granting that
// per-binding grants exist to prevent.
//
// WHY `invariant` AND NOT `assertion`. Both kinds observe. An assertion answers
// "did the thing I just did work?" and is bounded by that transition; these are
// scheduled, run forever on a cluster nobody is currently changing, and their
// failure means the platform DRIFTED rather than that a step failed. `operating`
// is the only state an invariant may attach to, which is the right answer here.
//
// WHY rotation-sla KEEPS secret-read AFTER #483. The grant was originally
// justified by the Loki OBJ-key exec, which is gone — but it was never the only
// thing that earned it. `secret-read` is defined as "read credential material OR
// ITS METADATA", and the lke-admin check lists Secret objects in kube-system and
// reads their creationTimestamps. Dropping the grant with the exec would make the
// DECLARATION the thing that is wrong, which is the failure mode declaring
// capabilities exists to prevent.
//
// WHY secret-read AND NOT secret-custody. When this was first declared the
// vocabulary had one word — secret-custody, documented as "read or write
// credential material" — so the declaration took it and said in a comment that it
// over-reported. The NEXT extension (token-inventory) could not make that trade:
// a read-only credential check was inexpressible, which forced the split.
// `secret-read` is what this lane always meant.
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
				Grants: []extension.Grant{extension.ClusterRead, extension.SecretRead},
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
