package assertsecrets

// extension.go — `assert-secrets` declares itself.
//
// TWENTY-SIXTH EXTENSION. Four lanes over the credential machinery: rotation SLAs
// and their metrics, the ESO round trip, the broad-PAT rotation drill, and the
// OpenBao audit log reaching Loki.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-secrets` declaration.
//
//	assertion:operating  "rotation-health" [cluster-read, secret-read]
//	assertion:verified   "eso-roundtrip"   [cluster-read, secret-read]
//	assertion:verified   "openbao-audit"   [cluster-read]
//	transition:converged "broad-pat-drill" [cluster-read, cluster-write]
//
// THE FOURTH BINDING, AND I WENT LOOKING FOR IT THIS TIME. The previous extraction
// found a `kubectl rollout restart` buried in a file called "readiness" — the
// third mutation hiding inside an observation — so the first thing done here was
// grep for writes rather than assume an `assert-` prefix meant read-only.
//
// It found one immediately: the broad-PAT rotation drill `kubectl apply`s a Job
// and `kubectl delete`s it afterward. That is unavoidable — you cannot assert a
// rotation ROTATES without running one — but it is a cluster write reached from a
// lane named `assert-`, and it is now declared as a transition.
//
// **Four for four.** Every extraction that has looked for a hidden mutation has
// found one (`converge`, `assert-storage`, `assert-observability`, this). The
// `assert-` prefix has turned out to be a statement of intent rather than of
// capability, which is exactly the gap the grant line closes — and the reason to
// grep before declaring is now settled practice rather than a lesson.
//
// WHY rotation-health IS AT `operating` AND THE REST ARE NOT. It reads
// llz_credential_age_days continuously and fails when a credential drifts past its
// SLA — drift detection against a live platform. The ESO round trip, the audit
// path and the drill each attest a property once, after a change.
//
// WHY secret-read AND NOT secret-custody. These lanes read credential material and
// its metadata to judge it; none of them PLACES any. The distinction
// `token-inventory` forced into the vocabulary, now used by its fifth extension.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-secrets",
		Short:  "credentials rotate on schedule, ESO delivers them, the drill works, and OpenBao's audit log lands",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "rotation-health",
				State:  extension.Operating,
				Grants: []extension.Grant{extension.ClusterRead, extension.SecretRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "eso-roundtrip",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead, extension.SecretRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "openbao-audit",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				// Applies and deletes a Job. You cannot assert a rotation ROTATES
				// without running one, so the write is inherent — but it is still a
				// write, and a lane named `assert-` is the last place a reader looks
				// for one.
				Kind:   extension.Transition,
				Name:   "broad-pat-drill",
				State:  extension.Converged,
				Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
			},
		},
	}
}
