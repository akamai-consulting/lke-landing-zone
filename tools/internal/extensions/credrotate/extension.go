package credrotate

// extension.go — TWO extensions declare themselves from one package, which no
// earlier extraction has needed.
//
// FORTY-FIRST AND FORTY-SECOND. `credential-pat` and `credential-objkey` are
// separate catalog rows and separate extensions — different credentials,
// different Linode APIs, independently enable-able. They share a PACKAGE because
// they share a FRAMEWORK: rotatorOpts' dry-run arming rule, the GitHub-secret
// fan-out across every infra-<deployment> environment, and the narrow Linode API
// slices. Go cannot put a shared type in two packages, so the framework decides
// where the rotators live; the registry takes functions rather than packages, so
// it does not decide what they declare.
//
// THE SECOND WALL, NOW DOWN. internal/baoread was the first: the credential family
// read OpenBao through package main's classifier. That extraction predicted the
// rows would unblock and they did not — they reached `credentials.go` too, which
// is this. Two walls, and the measurement says so plainly: `credentials_pat.go`
// went from unmeasurable-in-place to a closure of TWO once the framework moved.
//
// THE THIRD WALL IS ALSO DOWN, and it was the last one. `ci_rotate_linode_creds.go`'s
// rotation table — which credentials exist, where each lives in OpenBao, which
// Linode API mints it — and `linode_token.go`'s in-cluster token resolver are now
// here. Eighteen symbols with callers in mint-objkeys, incluster-pat,
// rotate-broad-pat, discover-firewall, volumes, reconcile, harbor-provisioner and
// seed-special: shared infrastructure in its own right, not one row's private code.
//
// WHAT THAT BOUGHT, MEASURED RATHER THAN CLAIMED. Every remaining file in the
// family dropped from unmeasurable-in-place to an ordinary closure:
//
//	ci_rotate_broad_pat.go   4      ci_temp_objkey.go    5
//	ci_seed_broad_pat.go     4      ci_mint_objkeys.go   6
//	ci_incluster_pat.go     10
//
// Three walls, and the third is the one that mattered most: it is the file that
// knows what a credential IS in this platform, so nothing that handles one could
// leave while it stayed.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// BOTH INCOMPLETE NOTES ARE GONE, AND BOTH PREDICTED THEIR OWN END. They recorded
// four paths as "still in package main — but the wall is GONE ... ordinary
// extractions from here, not blocked ones", and that is how it went:
// rotate-incluster-pat, mint-bootstrap-objkeys and temp-objkey are this package;
// rotate-broad-pat went to assertsecrets with the lane that drives it.
//
// Worth keeping the shape in mind: a note that says WHY something is blocked ages
// badly, because the reason expires before anyone re-reads the note. Four of the
// six notes making that claim across the catalog were stale when re-measured.
// PATExtension is the `credential-pat` declaration.
//
//	transition:seeded[cloud-mutate, secret-custody]
//
// It mints a Linode profile token, writes it into every infra-<deployment>
// GitHub Environment, and later revokes the superseded ones after a grace period.
// Custody because it PLACES the credential; cloud-mutate because minting and
// revoking are Linode API writes.
//
// `seeded` rather than `configured`, and here that is the HONEST state rather than
// a forced one — unlike `credential-state-passphrase`, which had to be pushed. The
// distinction is the one grantStates is defending: this token is ISSUED by Linode,
// so it cannot exist before there is an account to issue it, and seeding is
// exactly when the platform starts holding credentials it did not generate itself.
func PATExtension() extension.Extension {
	return extension.Extension{
		Name:   "credential-pat",
		Short:  "mint, distribute and retire the Linode PAT every deployment environment holds",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Seeded,
			Grants: []extension.Grant{extension.CloudMutate, extension.SecretCustody},
		}},
	}
}

// ObjKeyExtension is the `credential-objkey` declaration.
//
//	transition:seeded[cloud-mutate, secret-custody]
//
// The same shape over Object Storage keys, and deliberately a SEPARATE extension
// rather than a second binding on the first. The catalog's strongest structural
// signal is that a capability and its assertion enable together — but these two
// are neither: an instance with no object storage has no reason to rotate an OBJ
// key, and one that never issues PATs still needs its buckets. Collapsing them
// would tie two independent lifecycles to one switch.
func ObjKeyExtension() extension.Extension {
	return extension.Extension{
		Name:   "credential-objkey",
		Short:  "mint, distribute and retire the Object Storage keys the platform's buckets are read with",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Seeded,
			Grants: []extension.Grant{extension.CloudMutate, extension.SecretCustody},
		}},
	}
}
