package mutabletags

// extension.go — `guard-mutable-tags` declares itself.
//
// A GATE IN THE PLAINEST SENSE, like its neighbours: it reads one Actions
// workflow and judges the shape of the publish it describes. No cluster, no
// registry, no clock — `read-repo` and nothing else, which is what makes the gate
// kind legal for it.
//
// IT DELIBERATELY DOES NOT ASK GHCR. "Does `:latest` currently point at main's
// HEAD?" is the live question, and it is already answered live by
// `llz ci assert-image-fresh` at the first job of every instance pipeline. This
// gate holds the other half — the publish POLICY that makes that answer possible
// — at PR time, where the fix is a two-line edit rather than a repointed tag
// several open PRs have already resolved.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-mutable-tags` declaration.
//
//	gate:scaffolded[read-repo]
//
// `scaffolded`, not `built`: the property is decidable from the workflow file
// alone, and the cheapest moment to say so is before the commit that moves the
// publish — the same placement setup-go-sole-site argues for, and for the same
// reason. Waiting for a build would mean discovering it from a tag that has
// already moved.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-mutable-tags",
		Short:  "mutable image tags may be published only from the default branch's HEAD",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
