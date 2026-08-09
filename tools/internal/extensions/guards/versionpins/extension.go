package versionpins

// extension.go — `version-pins` declares itself: the gate that keeps every
// restatement of a pinned tool version equal to the one authority.
//
// FIFTY-SECOND EXTENSION. Its own subject is drift between copies of a version
// string, which makes it a fitting near-last member of a campaign whose most
// repeated finding was that copies drift — the file even carries a
// `versionAuthorityFile` const naming the one place a version is allowed to be
// decided.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `version-pins` declaration.
//
//	gate:scaffolded[read-repo]
//
// A gate, and the plainest one in the catalog: it reads files and compares
// strings. It never runs a tool, queries a registry, or asks whether the pinned
// version EXISTS — `pin-instance-images` and `chart-publish` do that, and keeping
// them separate is what lets this one stay a pure textual check with no network
// and no cluster.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "version-pins",
		Short:  "every restatement of a pinned tool version must equal the authority in dockerfiles/Dockerfile",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
