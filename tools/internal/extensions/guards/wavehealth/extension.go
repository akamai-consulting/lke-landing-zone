package wavehealth

// extension.go — `wave-health` declares itself.
//
// TWENTY-THIRD EXTENSION, and the SECOND state-level catalog correction. Like
// `posture-credential-coverage`, it is filed under `invariant: operating` — "the
// binding the current design has no room for" — and it reaches no cluster at all.
// Both checks are file scans over the repo's manifests; the package contains no
// exec, no HTTP and no kubectlprobe.
//
// The catalog's line count is wrong too, in the usual direction: it says 178 lines
// in 1 file, and it is 619 in 2. Seven corrections now, and every one was found by
// measuring rather than reading.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `wave-health` declaration.
//
//	gate:scaffolded[read-repo]
//
// TWO CHECKS, ONE BINDING, per the `guard-charts` rule — a split needs divergent
// CAPABILITY, not divergent subject matter:
//
//   - wave-health — a health-checked resource must not carry a negative sync-wave
//     annotation. Argo applies negative waves before the health gate can see them,
//     so the resource is created in a window where nothing is watching it.
//   - wave-dependency — a resource must not declare a wave EARLIER than something
//     it depends on.
//
// Both answer "are the sync waves in this repo coherent" at the same moment with
// the same grant.
//
// THE ALLOWLIST IS SHARED ACROSS A BOUNDARY, ON PURPOSE. `AllowedKinds` is
// exported because `assert-network`'s wave-health ValidatingAdmissionPolicy
// enforces the same rule in CEL at admission time, and the two must agree — a kind
// vetted here but not there passes into the cluster unchecked, and the reverse
// blocks a resource the guard would have allowed. `canary_test.go` is that
// coupling check, and it now runs ACROSS the two packages rather than inside one
// file, which is a stronger form of the same assertion.
//
// No ceiling change.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "wave-health",
		Short:  "sync waves are coherent: nothing health-checked runs before the gate, nothing runs before its dependency",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
