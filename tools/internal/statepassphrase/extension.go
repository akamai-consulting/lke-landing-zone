package statepassphrase

// extension.go — `credential-state-passphrase` declares itself, and is the first
// credential extension that could be extracted at all.
//
// FORTIETH EXTENSION. The catalog has six `credential-*` rows and five of them are
// BLOCKED behind the same wall: they read OpenBao through package main's
// bao_read.go, whose fail-closed verdict type and exec seam have seven and nine
// callers respectively. Extracting any one of them means extracting that layer
// first — the internal/keycloak shape, at a larger scale.
//
// THIS ONE IS NOT BLOCKED, AND THE REASON IS THE FINDING. The Terraform state
// encryption passphrase never enters OpenBao. It lives in a GitHub Environment
// secret, because the thing that needs it — `tofu init` in a reusable workflow —
// runs before any cluster exists to hold a secret store. A credential whose whole
// point is bootstrapping the substrate cannot be kept in the substrate.
//
// So the credential family is not one family. Five of its rows are OpenBao
// consumers; this one is a forge consumer, and it is the only one whose
// dependencies were already extractable.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `credential-state-passphrase` declaration.
//
//	transition:configured[read-repo, cloud-mutate, secret-custody]  ← REFUSED
//	transition:seeded[read-repo, cloud-mutate, secret-custody]
//
// THE STATE IS A FORCED MOVE, AND THIS IS THE SECOND TIME grantStates HAS PUSHED A
// BINDING RATHER THAN A GRANT. The honest state is `configured`: the passphrase is
// an INPUT that must resolve before the first `tofu init`, and it is written to a
// GitHub Environment, which is configured before Linode is provisioned. That is
// the exact argument `chart-publish` used to widen cloud-mutate to `configured`
// eight extractions ago.
//
// But this binding also PLACES CREDENTIAL MATERIAL — it mints a passphrase and
// stores it — so it needs `secret-custody`, and that row is
// {provisioned, seeded, operating}. Validate() refuses `configured`.
//
// AND THE ROW IS RIGHT TO REFUSE, which is why nothing was widened. Its comment
// states the rule plainly: custody at `scaffolded` or `configured` "would mean a
// credential exists before anything has been built to issue one, which is the
// shape of a hardcoded secret rather than a fetched one." This passphrase is
// GENERATED LOCALLY, not issued by anything — which is exactly the shape the row
// is defending against, and a real reviewer should look twice at it. Widening the
// row to accommodate the one credential that legitimately has no issuer would
// disarm the check for every credential that does.
//
// So `seeded` — the first state where custody is legal, and true enough: by the
// time anything can USE this passphrase, seeding has begun. The declaration loses
// the fact that it is written earlier, and that is the honest cost. This is a
// THIRD instance of the pattern `wedge-gameday` named — a binding pushed to the
// nearest legal state — and unlike that one it is pushed by the GRANT table rather
// than by bindableStates.
//
// `cloud-mutate` because writing a GitHub Environment secret is a mutation of
// infrastructure this repo does not contain — the meaning `env-topology`
// established and `chart-publish` and `release-publish` have since used.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "credential-state-passphrase",
		Short:  "mint and roll over the Tofu state-encryption passphrase, which lives outside OpenBao by necessity",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Seeded,
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudMutate, extension.SecretCustody},
		}},
		Incomplete: []string{
			"the binding is at `seeded` because grantStates refuses secret-custody at " +
				"`configured`, which is where this actually runs — the passphrase must exist " +
				"before the first `tofu init`. The row is right to refuse: it defends against " +
				"credentials that exist before anything can issue them, and this one is " +
				"generated locally with no issuer, which is precisely that shape. Widening the " +
				"row for the one credential that legitimately has no issuer would disarm the " +
				"check for every credential that does. Recorded rather than fixed.",
		},
	}
}
