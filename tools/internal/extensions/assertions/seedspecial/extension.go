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
// this campaign has accepted and is recorded as such: both run in the seeding
// window, both read the cluster, neither writes. If either grows a write or a
// second caller, split them — there is no deeper unity here of the kind that kept
// `openbao-peer-ca`'s two halves or Harbor's active/standby pair together.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `seed-special` declaration.
//
//	assertion:seeded[cluster-read]
//
// AN ASSERTION, NOT A TRANSITION, and the PVC audit is why. It reports and
// classifies; it does not repair. The classification is the interesting part:
// `kyvernoScopedNamespaces` splits findings into "Kyverno's webhook covers this
// namespace, so a wrong StorageClass means the webhook lagged" and "it does not,
// so this needs a different explanation". Attributing an out-of-scope PVC to a
// timing bug sends the reader after something that cannot explain it, which is
// why TestKyvernoScopeMatchesPolicy pins that list against the ClusterPolicy
// itself.
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
