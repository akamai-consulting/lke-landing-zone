package assertplatform

// extension.go — `assert-platform` declares itself.
//
// FIFTEENTH EXTENSION, AND THE FIRST THAT IS PURELY ASSERTIONS. Four lanes that
// observe a platform someone else built and report whether it is what it claims
// to be. Nothing here mutates, and the declaration is four bindings holding one
// read grant each — which is what an assertion-only extension is supposed to look
// like, and worth having one of on the record now that the mutating shapes are
// all exercised.
//
// THE CATALOG NAMED FIVE FILES; FOUR BELONG. `ci_assert_image_fresh.go` stayed in
// package main: its closure is the TEMPLATE-PIN machinery (assertPinCoherence,
// pinnedTemplateRef, resolveTemplateCommit), not platform health. It asserts that
// an instance's pinned template ref and its images agree — a `template-sustain`
// question wearing an `assert-` filename. Fourth time the catalog's file list has
// been wrong, and the fourth time for the same reason: it grouped by name.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-platform` declaration.
//
//	assertion:verified "health-workflow"  [cluster-read]
//	assertion:verified "argo-app"         [cluster-read]
//	assertion:verified "instance-custom"  [cluster-read]
//	assertion:configured "apl-version"    [read-repo]
//
// WHY THE LAST ONE BINDS A DIFFERENT STATE. Three of these run a cluster and read
// what is there. `assert-apl-version` does not: it reads the instance's pinned
// apl-core chart version out of the SPEC FILE and compares it against the floor
// this llz supports. There is no cluster involved and none needs to exist — it is
// a statement about how the instance is CONFIGURED, and it is deliberately
// runnable before anything is provisioned, because refusing an unsupported chart
// after a 45-minute bootstrap is the failure it exists to prevent.
//
// That is the same argument `token-inventory`'s validate-tokens lane makes, and
// the same shape: a preflight is not a gate just because it blocks. It reads more
// than files, so it is an assertion; it reads them before provisioning, so it
// binds `configured`.
//
// FOUR BINDINGS, NOT ONE. `guard-charts` established that a split needs divergent
// CAPABILITY rather than count, and three of these do hold identical grants — so
// on that rule alone they could collapse. They are named separately because their
// STATES differ (apl-version is `configured`, the rest are `verified`), and once
// the set is split at all, naming the three siblings is what keeps the listing
// legible. Collapsing the three would also hide that they fail independently:
// each is wired into a different CI lane and a reader of `llz extension list`
// should see four things that can go red, not one.
//
// No ceiling change. Assertions may bind any state and `cluster-read`/`read-repo`
// are unrestricted.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-platform",
		Short:  "assert the platform is what it claims: workflows run, apps sync, customisations land, the chart is supported",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "health-workflow",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "argo-app",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "instance-custom",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "apl-version",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo},
			},
		},
	}
}
