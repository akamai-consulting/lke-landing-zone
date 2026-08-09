package openbao

// extension.go — `openbao-login` declares itself: the two ways a caller obtains an
// OpenBao token, now in the package that owns the client.
//
// SIXTY-SIXTH EXTENSION. `openbao_login.go` (the interactive `llz openbao login
// --team`) and `ci_openbao_login.go` (the in-cluster CI login) measured 6 outbound
// together, every one noise or a one-field globalOpts read.
//
// THEY ARRIVED WITH A NAME COLLISION WAITING TO HAPPEN: `runOpenbaoLogin` and
// `runOpenBaoLogin`, differing by ONE CAPITAL, in two files that had never shared a
// package. They are `RunTeamLogin` and `RunCILogin` now. Two symbols one keystroke
// apart is how a caller reaches for the wrong one and gets a plausible answer —
// the same call made for `contains`/`sliceContains` in this package earlier.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `openbao-login` declaration.
//
//	transition:seeded[read-repo, cluster-read, secret-custody]
//
// A TRANSITION, NOT AN ASSERTION, and the reason is the last line of both lanes:
// each MINTS a short-lived token and puts it somewhere — the CI lane writes it to
// $GITHUB_ENV masked, the team lane exports it for a shell. Obtaining a credential
// and placing it is `secret-custody` at `seeded`, not evidence about a state.
//
// `read-repo` for the spec (the team lane derives the realm issuer from the
// region's domainSuffix); `cluster-read` because both must reach the in-cluster
// listener, and the managed path reads apl-core's own otomi-api ConfigMap to
// discover the issuer rather than making one up.
//
// A CYCLE IS DECLARED HERE AS A SEAM. That discovery call used to be a direct
// import of internal/identityconfig, which is a cycle in the TEST build:
// identityconfig(test) → reconcilelanes → openbao → identityconfig. Go reports it
// as "import cycle not allowed in test", which reads like a test problem and is
// not: it is the build saying the identity plane and the OpenBao client both want
// to own "which Keycloak issues our tokens". DiscoverIssuerFromCluster is a var
// defaulting to "" — the client asks, package main answers.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "openbao-login",
		Short:  "obtain a short-lived OpenBao token, interactively by team or in-cluster for CI",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			State: extension.Seeded,
			Grants: []extension.Grant{
				extension.ReadRepo, extension.ClusterRead, extension.SecretCustody,
			},
		}},
	}
}
