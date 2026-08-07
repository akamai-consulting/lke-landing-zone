package identityconfig

// extension.go — `identity-plane` declares itself: the four lanes that make
// Keycloak and OpenBao agree about who anyone is.
//
// FORTY-EIGHTH EXTENSION, AND THE LARGEST SINGLE SET IN THE CAMPAIGN — 1,713
// lines across four files. It moved on a measurement that looks, at a glance,
// like it says nothing:
//
//	3 files                       11 outbound
//	3 files + openbao_configure   11 outbound
//
// The COUNT did not change. The SHAPE did. Without ci_openbao_configure.go, three
// of the eleven were `discoverManagedDomain`, `keycloakIssuerFor` and `specTeams`
// — real couplings to a file staying behind, coupled in BOTH directions
// (`keycloakDeviceClientID` came back the other way). With it, every edge is an
// exec adapter or a one-line helper, and all four went into deps.go. That is the
// lesson this extraction contributes: judge a candidate by whether its edges are
// CUTTABLE, not by how many there are.
//
// WHY THESE FOUR ARE ONE EXTENSION. They are the two halves of a single fact —
// that a person or a workflow has an identity both systems recognise:
//
//	keycloak-configure     the realm, the device-flow client, the team roles
//	openbao-configure      the policies and the JWT auth backend that TRUSTS
//	                       that realm's issuer
//	users-add              puts a human into it
//	gateway-alias          pins the DNS the issuer URL resolves through
//
// Splitting them would mean an extension that configures a trust relationship
// without the thing it trusts. `keycloakIssuerFor` being reached for across the
// old file boundary was not an accident of layout; it was this fact showing
// through.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `identity-plane` declaration.
//
//	transition:seeded[read-repo, cluster-read, cluster-write, secret-custody]
//
// `seeded`, not `provisioned`. Keycloak and OpenBao are both running before any
// of this executes — what these lanes place is CREDENTIALS AND TRUST: an admin
// secret read out of the cluster, policies written into OpenBao, a JWT auth mount
// pointed at Keycloak's JWKS, and a human user with roles.
//
// `cluster-write` is here for exactly ONE call and it is worth naming, because
// the package is otherwise all reads: `patchWithWebhookRetry` PATCHes the
// Keycloak StatefulSet's hostAliases so the issuer hostname resolves to the
// gateway from inside the pod. It retries around a Kyverno webhook rejection,
// which is the shape this repo has been bitten by before — the admission
// starvation recorded in the sc-default-demote race.
//
// `read-repo` is for the spec: `SpecTeams` reads the declared team list to
// typo-guard a `--team` that was never declared.
//
// NOTE WHAT IS ABSENT. No `cloud-mutate`, despite `forgeFromEnv`: the forge is
// read to DERIVE the OIDC discovery URL and bound issuer for GitHub Actions
// (`forge.OpenBaoJWTAuthConfig`), and on GHES those differ from github.com. It is
// a fact about the forge being consulted, not a change being made to it.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "identity-plane",
		Short:  "configure the Keycloak realm and the OpenBao policies and JWT trust that follow from it",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			Name:  "identity-config",
			State: extension.Seeded,
			Grants: []extension.Grant{
				extension.ReadRepo, extension.ClusterRead,
				extension.ClusterWrite, extension.SecretCustody,
			},
		}},
	}
}
