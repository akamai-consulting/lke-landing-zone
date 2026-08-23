package k8sminorcoherence

// extension.go — `k8s-minor-coherence` declares itself: the gate that keeps the
// Kubernetes minor CI validates against equal to the one we deploy.
//
// A GATE, AND ONE THAT COULD ONLY EVER HAVE BEEN A GATE. Its subject is a
// relation between two files in this tree — a tfvars pin and a workflow's kind
// node image — so it needs no cluster, no network and no clock, and `read-repo`
// is the whole of what it may hold. That is also why it is worth having at all:
// the thing it protects (a server-side dry-run's fidelity) is only observable
// from a cluster, but the defect that destroys it is fully decidable from the
// repo.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `k8s-minor-coherence` declaration.
//
//	gate:scaffolded[read-repo]
//
// SIBLING TO version-pins, NOT A MEMBER OF IT. Both are relations between
// restatements of a version, and the temptation was to add one more class to
// that guard. They have different authorities and different comparisons:
// version-pins holds every copy EQUAL to an ARG in dockerfiles/Dockerfile, while
// this one holds a workflow's node image to the MINOR of an LKE-Enterprise build
// id in a Terraform root's tfvars example — a value no Dockerfile has an opinion
// about, compared at a precision equality cannot express (kind ships v1.34.8;
// Linode offers v1.34.6+lke2; those must not be equal and must not diverge).
// Folding it in would have made version-pins' one-sentence contract false.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "k8s-minor-coherence",
		Short:  "lint.yml's kind node image must run the Kubernetes minor the cluster root pins for LKE-Enterprise",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
		Incomplete: []string{
			"the `branches:` half of the trigger is unread. unreachableOnChange holds " +
				"`paths:`, `paths-ignore:` and the existence of a contributor-raised event, " +
				"but lint.yml targets `branches: [main, master]` — so a STACKED PR based on " +
				"a feature branch starts no Lint run at all and this gate prints nothing. " +
				"Statically decidable, and deliberately not enforced: targeting main is the " +
				"correct configuration, so there is no filter to demand — the exposure is " +
				"the stacking, which every gate in that workflow shares and which the " +
				"branch-base guidance already covers.",
			"it does not run on a FORK PR, and the door it leaves is the one its own " +
				"doctrine closes everywhere else. It reaches CI through `llz ci gates` in " +
				"lint.yml's `kubernetes` job, which is `if:`-gated off for forks because it " +
				"needs a private GHCR image — while the `dry-run` job it vouches for runs " +
				"there anyway. Two other guards were relocated to the fork-safe `go-tests` " +
				"job for exactly this, and the same move would fix this one; it is not made " +
				"here because it would move all twenty-nine gates, which is a change about " +
				"the driver rather than about this gate. Same-repo PRs and the push trigger " +
				"still catch it, so it is a detection delay rather than a hole.",
		},
	}
}
