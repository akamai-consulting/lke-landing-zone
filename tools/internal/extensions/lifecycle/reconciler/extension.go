package reconciler

// extension.go — `reconciler-runtime` declares itself: the long-running process
// that HOSTS the reconcile lanes, plus the four lanes that live in it.
//
// FORTY-NINTH EXTENSION, AND THE ONLY DAEMON IN THE TREE. Everything else in this
// campaign is a command that runs, does one thing and exits. This is the manager
// loop, the leader election, the health and metrics servers, and the samplers that
// keep them honest — 2,758 lines that never terminate on purpose.
//
// IT PAIRS WITH `reconcile-actions` (internal/reconcilelanes) AND DOES NOT ABSORB
// IT. That extension declares four lanes as four separate invariants precisely so
// their grants do not collapse into a union; folding them into their host would
// undo that in one move. The split here is the same discipline one level up: the
// runtime declares what IT needs, and each lane it hosts declares its own.
//
// WHY THE COBRA CONSTRUCTORS STAYED (see cmds.go): `reconcileOpts` has 25 fields,
// twenty of them a `--reconcile-<lane>` bool paired with a resync interval. That
// struct is the runtime's configuration surface, not wiring around it, and
// exporting all 25 so package main could set them would trade a private struct for
// a public one and buy nothing. First departure from the flag-sets-go-back rule,
// argued rather than assumed.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `reconciler-runtime` declaration.
//
//	invariant:operating [cloud-read, cluster-read, cluster-write]  cidr-firewall
//	invariant:operating [cloud-read, cloud-mutate, cluster-read]   volume-hygiene
//	invariant:operating [cluster-read, cloud-mutate]               apl-overlay-sync
//	invariant:operating [cluster-read, secret-custody]             runtime-gauges
//
// FOUR BINDINGS, FOUR DIFFERENT GRANT SETS, and that is the whole argument for not
// writing one. Collapse them and the union is every mutating grant the model has —
// the metrics sampler would carry permission to retag Linode Volumes and commit to
// the apl-values repo. Each lane below holds exactly what it uses:
//
//   - cidr-firewall reads the Linode firewall's CIDRs and writes them into a
//     kube-system ConfigMap, then rolls the controller Deployment. Cloud in,
//     cluster out — the only binding here that reads one substrate and writes
//     another.
//   - volume-hygiene labels and tags Linode Volumes so an orphan is attributable.
//     It never writes to the cluster; the cluster is where it learns which volumes
//     exist.
//   - apl-overlay-sync commits to the apl-values repo.
//   - runtime-gauges is the health, convergence and token-inventory sampling. It
//     holds an OpenBao credential to sample what OpenBao knows and writes nothing
//     anywhere, which is why it gets `secret-custody` and no write grant at all.
//
// `cloud-mutate` FOR A GIT COMMIT, and this is the finding worth recording.
// `write-repo` exists and is the obviously-matching word — but its states are
// {scaffolded, upgraded}, the two moments copier runs, and this commits at
// `operating`. The refusal is right rather than inconvenient: what copier does is
// write FILES INTO A WORKING TREE, and what this does is call the GitHub git-data
// API. Same target, different mechanism, different blast radius — an API commit
// needs a token that a working-tree write does not. So the model distinguishes
// repo writes by MECHANISM AND MOMENT, not by target, and `cloud-mutate` (which
// already covers every other GitHub API write in the catalog — chart-publish, the
// Harbor secret publishes, the OpenBao root-token stores) is the honest word.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "reconciler-runtime",
		Short:  "the in-cluster reconcile daemon: leader election, lane scheduling, health and metrics",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind: extension.Invariant, Name: "cidr-firewall", State: extension.Operating,
				Grants: []extension.Grant{extension.CloudRead, extension.ClusterRead, extension.ClusterWrite},
			},
			{
				Kind: extension.Invariant, Name: "volume-hygiene", State: extension.Operating,
				Grants: []extension.Grant{extension.CloudRead, extension.CloudMutate, extension.ClusterRead},
			},
			{
				Kind: extension.Invariant, Name: "apl-overlay-sync", State: extension.Operating,
				Grants: []extension.Grant{extension.ClusterRead, extension.CloudMutate},
			},
			{
				Kind: extension.Invariant, Name: "runtime-gauges", State: extension.Operating,
				Grants: []extension.Grant{extension.ClusterRead, extension.SecretCustody},
			},
		},
	}
}

// cloudBinding is the binding a Linode call is made under, chosen by NAME because
// this extension declares several with DIFFERENT cloud permissions.
//
// THE RULE IS THE NARROWEST BINDING THAT COVERS WHAT THE CALL ACTUALLY DOES, and
// it is applied by reading the HTTP verb rather than the function name. `discover_firewall.go` calls ListVPCSubnets — a GET — so it takes
// `cidr-firewall`, the read invariant, not the one that also holds cloud-mutate.
func cloudBinding(name string) extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == name {
			return b
		}
	}
	panic("llz-reconciler: no binding named " + name + " — its Linode client is built from one, " +
		"so its absence is a wiring bug")
}
