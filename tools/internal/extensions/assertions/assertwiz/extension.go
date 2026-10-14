// Package assertwiz is the `assert-wiz` extension: the e2e/scheduled gate that
// the `wiz` component's cluster enrollment still WORKS — not that its manifests
// rendered or its Application reads Synced.
package assertwiz

// extension.go — `assert-wiz` declares itself.
//
// It follows the `wiz` component in both directions: an instance (or the e2e
// deployment) without Wiz has no tenant and no credential, so the assert battery
// reports the lane SKIPPED with the reason "component wiz disabled" — visibly,
// in the lane's own ::group:: header and in the closing count — rather than
// running a gate that could only ever fail, or one that self-skips and is
// counted as a pass.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-wiz` declaration.
//
//	assertion:verified "wiz-enrollment" [cluster-read, secret-read]
//
// WHY secret-read. The gate decodes two Secrets to judge them: the credential
// (are clientId and clientToken non-empty — never printed) and the registration
// record the upstream Job writes once Wiz has ACCEPTED the cluster. Reading
// credential material is exactly the half of the old secret-custody word that a
// read-only assertion may hold.
func Extension() extension.Extension {
	return extension.Extension{
		Name:  "assert-wiz",
		Short: "fail unless the cluster is enrolled in Wiz: credential delivered, connector registered, broker tunnel Ready",
		// OPT-IN, by component: no Wiz tenant, nothing to enroll.
		Component: "wiz",
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			Name:   "wiz-enrollment",
			State:  extension.Verified,
			Grants: []extension.Grant{extension.ClusterRead, extension.SecretRead},
		}},
	}
}
