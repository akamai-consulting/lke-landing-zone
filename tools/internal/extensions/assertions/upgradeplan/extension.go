package upgradeplan

// extension.go — `assert-upgrade-plan` declares itself.
//
// AN ASSERTION, NOT A GATE, and the distinction is about WHEN it can answer. A
// gate is decidable from repo contents; this one needs a plan taken against live
// state an earlier release created, which exists only inside a pipeline that has
// stood a cluster up.
//
// `cloud-read` WAS ADDED FOR ONE GET, and the narrowest one available. Whether a
// destructive bucket plan is data loss or a harmless rename is not in the plan —
// both look identical there — and Linode answers it in the bucket listing, which
// carries an `objects` count per bucket. Without it the assertion has to refuse
// every rename, which is safe and also makes the correct migration unperformable.
//
// READ AND NOT MUTATE, structurally: capability's policy refuses every mutating
// method on this client, so the strongest thing a bug here can do is read the
// account's bucket list. That matters because this runs between a plan and an
// apply, holding the token that could delete the very buckets it is protecting.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-upgrade-plan` declaration.
//
//	assertion:configured[cloud-read,read-repo]
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-upgrade-plan",
		Short:  "an upgrade must not propose destroying or replacing a live resource",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
		}},
	}
}
