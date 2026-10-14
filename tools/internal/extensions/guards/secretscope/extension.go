package secretscope

// extension.go — `workflow-secret-scope` declares itself.
//
// It reads Actions YAML and the requirement table, and compares them. No cluster,
// no network, no clock — so `read-repo` and nothing else.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `workflow-secret-scope` declaration.
//
//	gate:scaffolded[read-repo]
//
// `scaffolded` because the property is decidable from the tree, and it has to be
// caught there: at runtime a secret that did not resolve is the empty string, and
// what the operator sees is whatever the tool does with an empty credential —
// `tofu init` failing on the S3 backend, three layers from the cause.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "workflow-secret-scope",
		Short:  "an environment-scoped secret resolves only inside an infra-<env> job, and never on a pull request",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
