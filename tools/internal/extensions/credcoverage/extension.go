package credcoverage

// extension.go — `posture-credential-coverage` declares itself.
//
// NINETEENTH EXTENSION, AND THE CATALOG PUT IT IN THE WRONG SECTION. It is filed
// under `invariant: operating` — "the binding the current design has no room for;
// without it these 4,283 lines stay core-special" — and it is neither an invariant
// nor at `operating`. It reaches no cluster and no cloud. Both of its checks are
// FILE SCANS over the repo's manifests, and the only I/O in the package is
// os.ReadFile.
//
// FIFTH CATALOG CORRECTION, AND THE FIRST ABOUT A STATE RATHER THAN A FILE LIST.
// The previous four (guard-docs, health-sla, token-inventory, assert-platform)
// were all "these files do not belong together". This one groups the right files
// and puts them at the wrong moment — which is a more interesting error, because
// the state is what a reader uses to know WHEN a check runs. Filed at `operating`
// it reads as continuous drift-detection against a live platform; it is actually a
// pre-commit file check that needs no platform at all.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `posture-credential-coverage` declaration.
//
//	gate:scaffolded[read-repo]
//
// TWO CHECKS, ONE BINDING, following the rule `guard-charts` established: a split
// needs divergent CAPABILITY, not divergent subject matter. These hold the same
// grant at the same moment and answer one question in two halves —
//
//   - externalsecret-paths — every ExternalSecret `remoteRef` in the manifests
//     points at a path something actually seeds.
//   - credential-coverage-guard — every credential the platform holds is measured
//     by something, so none rots unwatched.
//
// — which is "is every credential in this repo accounted for, both ends". One
// question, one moment, one binding.
//
// IT IS A GATE, WHICH IS THE STRICTEST THING TO CLAIM HERE. A gate may hold
// `read-repo` and nothing else, because it is defined by COST AND REACH: fast,
// local, files only. `token-inventory`'s validate-tokens lane failed that test
// (it probes GitHub, Linode and S3 over the network, so it is an assertion). This
// one passes it — and the passing is checked, not asserted: see
// TestPackageStaysFilesOnly, which fails if this package ever grows a cluster
// client, a network call or a write path.
//
// The catalog also marks it `ext? ✔` — plausibly externalisable as a pure-argv
// action later. That reading survives the extraction: nothing here holds an
// in-process Go handle.
//
// No ceiling change.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "posture-credential-coverage",
		Short:  "every ExternalSecret path resolves, and every credential is measured by something",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
