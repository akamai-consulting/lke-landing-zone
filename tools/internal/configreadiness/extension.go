package configreadiness

// extension.go — `config-readiness` declares itself.
//
// TWENTIETH EXTENSION, and the catalog called it right: "**This is the
// `configured` predicate** — the cleanest existing example of predicate code
// that's mis-filed as a command." It is, and it needed no correction.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `config-readiness` declaration.
//
//	assertion:configured[read-repo, cloud-read, secret-read]
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
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "config-readiness",
		Short:  "is this instance configured well enough to build: credentials set, tfvars present, no sentinels left",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Assertion,
			State: extension.Configured,
			Grants: []extension.Grant{
				extension.ReadRepo,
				extension.CloudRead,
				extension.SecretRead,
			},
		}},
	}
}
