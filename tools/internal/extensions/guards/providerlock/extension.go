package providerlock

// extension.go — `guard-provider-lock` declares itself.
//
// A GATE IN THE PLAINEST SENSE: it reads four kinds of file out of this repo —
// the embedded roots' versions.tf, the modules' versions.tf, the roots' main.tf
// (for which modules they compose) and the delivered .terraform.lock.hcl — and
// compares two numbers. No cluster, no registry, no network, no clock.
// `read-repo` and nothing else, which is what makes the gate kind legal for it.
//
// SCAFFOLDED, NOT BUILT, and the placement is the point. The cheapest moment to
// learn that a constraint bump strands every existing instance is the PR that
// writes the bump, where the fix is regenerating the lock in the same commit.
// Every later moment — a release cut, an adopter's `tofu init` — is one where the
// tag is already published and the recovery is manual, per instance.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-provider-lock` declaration.
//
//	gate:scaffolded[read-repo]
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-provider-lock",
		Short:  "delivered .terraform.lock.hcl pins must satisfy the provider constraints the template ships",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
