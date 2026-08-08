package meshegress

// extension.go — `mesh-egress` declares itself.
//
// FOUND BY A SWEEP, NOT A HUNCH. This file measured CLOSURE 1 — `main`, and
// nothing else — and had done so for the whole campaign. It was found only after
// two "mechanical extraction is complete" calls were disproved by re-measuring,
// which is why the state file now opens every iteration with the per-file sweep.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `mesh-egress` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, and one earned by an outage. apl-core runs the platform namespaces under
// an Istio sidecar with STRICT mTLS, so a plaintext request from a pod OUTSIDE the
// mesh is rejected at the sidecar regardless of NetworkPolicy — which is a failure
// that looks like a network problem and is not one. This reads rendered charts and
// says so before the cluster does.
//
// `read-repo` and nothing else, which is what makes the gate kind legal:
// Validate() refuses a gate holding any other grant, and this one opens files and
// compares strings with no cluster and no cloud.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "mesh-egress",
		Short:  "every plaintext hop into a STRICT-mTLS namespace must come from inside the mesh",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
