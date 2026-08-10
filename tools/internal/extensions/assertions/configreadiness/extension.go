package configreadiness

// extension.go — `config-readiness` declares itself.
//
// TWENTIETH EXTENSION, and the catalog called it right: "**This is the
// `configured` predicate** — the cleanest existing example of predicate code
// that's mis-filed as a command." It is, and it needed no correction.
//
// IT NOW CARRIES A SECOND BINDING IT DID NOT AUTHOR. `instance-resolve` used to be
// its own extension: a declaration and nothing else, after its four resolvers moved
// to internal/shared/instanceresolve. A package holding a declaration and no code
// is not an extension, it is a label on a library — so the package went and the
// CLAIM came here, because the claim was true and deleting it would have removed
// the record while leaving the behaviour.
//
// WHY HERE AND NOT SOMEWHERE ELSE. Six packages import the resolvers (envadd,
// newinstance, onboard, reachability, render, teardown), so ownership could not be
// settled by counting callers. It is settled by what the two claims SAY: both ask
// "does this instance's configuration resolve?", one at `scaffolded` over inputs
// and one at `configured` over credentials and tfvars. They are one predicate
// measured twice, which is exactly what a multi-binding extension is for.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `config-readiness` declaration.
//
//	assertion:scaffolded/inputs-resolve[read-repo, cloud-read]
//	assertion:configured/build-ready  [read-repo, cloud-read, secret-read]
//
// WHY assertion AND NOT gate. It reads the repo, but it also asks GitHub which
// secrets and variables are actually set, and Linode whether the account is
// reachable. A gate is defined by cost and reach — fast, local, files only — so
// the moment a check leaves the filesystem it is an assertion. Same line
// `token-inventory` and `assert-platform` drew.
//
// WHY secret-read. It reports which credentials are configured, which means
// reading GitHub's secret NAMES (never values — the API does not expose them) and
// the local .llz cache, which does hold values. That is reading credential
// material, not placing it: the distinction `token-inventory` forced into the
// vocabulary, and the third extension to need the read half.
//
// WHY `configured` IS THE POINT. This extension answers exactly one question —
// is this instance configured well enough to build? — and every other state
// presumes the answer is yes. It is the predicate the state is named for.
//
// WHY `inputs-resolve` IS AN ASSERTION AND NOT A GATE — recorded because the
// original author tried the gate first and Validate() refused it. The gate rule
// does not merely bar mutating grants; it permits `read-repo` and nothing else,
// "it runs in the fast pre-commit path over files alone". `region_resolve` and
// `objcluster_resolve` call the Linode API, and that is the point of them — the
// hardcoded region list they replaced went stale. Something that needs the network
// is not a gate no matter how read-only it is.
//
// NO `secret-read` ON THE SCAFFOLDED BINDING, deliberately. The resolvers read a
// repo and a cloud; only the `configured` half reads credential material. Folding
// the two into one binding would have widened the union and handed the resolvers a
// grant they never use — the same over-granting argument `reconcile-actions` made
// when it split into four.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "config-readiness",
		Short:  "does this instance resolve and is it configured well enough to build: inputs, credentials, tfvars, no sentinels left",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Scaffolded,
			Name:   "inputs-resolve",
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
		}, {
			Kind:  extension.Assertion,
			State: extension.Configured,
			Name:  "build-ready",
			Grants: []extension.Grant{
				extension.ReadRepo,
				extension.CloudRead,
				extension.SecretRead,
			},
		}},
	}
}

// cloudBinding is `inputs-resolve`, the assertion whose job is asking Linode
// whether the spec's region, node type and object-storage cluster resolve.
//
// BOTH cloud-bearing bindings here hold cloud-read and NOTHING else, so the
// permission is identical whichever is chosen and no over-granting is possible.
// It is named anyway: the binding is the unit the model reasons about, and a
// reader tracing "which declaration permits this call" should land on the one
// whose description matches what the call does.
func cloudBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == "inputs-resolve" {
			return b
		}
	}
	panic("config-readiness: no assertion:inputs-resolve binding — the Linode " +
		"preflight builds its client from it, so its absence is a wiring bug")
}
