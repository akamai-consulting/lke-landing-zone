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
// THERE IS A THIRD WALL, and it is recorded rather than guessed at.
// `credential-linode` (the LKE-admin rotator, moved here) and `credential-objkey`'s
// mint path still reach `ci_rotate_linode_creds.go`'s rotation table and
// `linode_token.go`'s in-cluster token layer — eighteen symbols with callers in
// mint-objkeys, incluster-pat, rotate-broad-pat, discover-firewall, volumes,
// reconcile, harbor-provisioner and seed-special. That is shared infrastructure in
// its own right, not one row's private code.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

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
		Incomplete: []string{
			"the in-cluster PAT path (ci_incluster_pat.go) and the broad-PAT rotation drill " +
				"stay in package main: both reach ci_rotate_linode_creds.go's rotation table and " +
				"linode_token.go's in-cluster token layer, which are shared infrastructure with " +
				"callers across eight other files rather than this extension's private code.",
		},
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
		Incomplete: []string{
			"the mint path (ci_mint_objkeys.go), the temp-key path (ci_temp_objkey.go) and the " +
				"obj-cluster resolver stay in package main: they reach the rotation table in " +
				"ci_rotate_linode_creds.go, which is shared infrastructure and the third wall " +
				"this family sits behind.",
		},
	}
}
