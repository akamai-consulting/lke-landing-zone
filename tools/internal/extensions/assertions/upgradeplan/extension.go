package upgradeplan

// extension.go — `assert-upgrade-plan` declares itself.
//
// AN ASSERTION, NOT A GATE, and the distinction is about WHEN it can answer. A
// gate is decidable from repo contents; this one needs a plan taken against live
// state an earlier release created, which exists only inside a pipeline that has
// stood a cluster up. It reads that plan as a file, so `read-repo` is the whole
// of what it touches — no cluster handle, no cloud credential, no clock.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-upgrade-plan` declaration.
//
//	assertion:configured[read-repo]
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-upgrade-plan",
		Short:  "an upgrade must not propose destroying or replacing a live resource",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
