package seedspecial

// extension.go — `seed-special` declares itself: two small lanes that share a
// moment rather than a mechanism.
//
// FIFTY-FOURTH EXTENSION. `resolve-harbor-url` reads the managed Harbor hostname
// out of the cluster and emits it as a step output; `audit-pvc-storage-class`
// reports which PVCs landed on the wrong StorageClass and splits them by whether
// Kyverno's policy covers their namespace.
//
// THEY ARE ONE EXTENSION BECAUSE THEY SHARE A MOMENT, which is the weakest reason
// this tree has accepted and is recorded as such: both run in the seeding
// window, both read the cluster, neither writes. If either grows a write or a
// second caller, split them — there is no deeper unity here of the kind that kept
// `openbao-peer-ca`'s two halves or Harbor's active/standby pair together.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `seed-special` declaration.
//
//	assertion:seeded[cluster-read]
//
// AN ASSERTION, NOT A TRANSITION, and the PVC audit is why. It reports and
// classifies; it does not repair. The classification used to be the interesting
// part, and it was classifying against a mechanism that had stopped existing: a
// `kyvernoScopedNamespaces` list split findings into "Kyverno's webhook covers
// this namespace, so a wrong StorageClass means the webhook lagged" and "it does
// not". That policy has not been applied since LLZ went managed-only, so the first
// branch sent readers after a timing bug that could not have happened. Both the
// list and the coupling test that faithfully pinned it to the policy are gone; the
// audit now reports the one cause that is real, StorageClass ordering.
//
// `cluster-read` and nothing else — verified by reading, not assumed: every call
// in the package is a `kubectl get`, and the only writes anywhere near it are the
// GitHub step outputs, which are this process describing its own findings.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "seed-special",
		Short:  "resolve the managed Harbor URL, and audit PVCs that landed on the wrong StorageClass",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Seeded,
			Grants: []extension.Grant{extension.ClusterRead},
		}},
		Incomplete: []string{
			"the two lanes share a MOMENT, not a mechanism — the weakest grouping " +
				"reason in this catalog. Split them if either grows a write or a " +
				"second caller.",
		},
	}
}
