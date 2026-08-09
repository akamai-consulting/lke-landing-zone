package bootstrapcluster

// extension.go — `bootstrap-cluster` declares itself: the lane that turns a bare
// LKE cluster into one apl-core is converging on.
//
// FIFTY-SIXTH EXTENSION AND THE LAST BIG CLEAN SET — 1,702 lines. It looked
// entangled for the whole campaign and was not: measured ALONE,
// ci_bootstrap_cluster.go comes back 15 outbound, and EIGHT of those fifteen are
// its own manifests sibling. Add that file and ci_prepare_apl_upgrade.go and it
// is 6, then 5 once `loadSpec` moved to internal/clusterspec.
//
// That is the trap worth naming last, because it is the mirror of the one this
// campaign started with: a per-file closure understates a set by hiding shared
// helpers, AND overstates it by counting a sibling as a foreign dependency. Check
// where a file's edges POINT before calling it tangled.
//
// THE MANIFESTS CAME TOO, and that moved a documented constraint rather than
// solving it. internal/kyverno's extension.go had recorded that the policy assets
// could not follow it because `//go:embed` pins data to its package directory —
// so they stayed with the embedder. They still do; the embedder is just no longer
// in `package main`. Its TestManifestsStayWithTheEmbeddingPackage caught the move,
// which is exactly the job it was written for.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `bootstrap-cluster` declaration.
//
//	transition:provisioned[read-repo, cluster-read, cluster-write, secret-custody]
//
// `provisioned` IS THE STATE THIS LANE CREATES. The cluster exists — Terraform
// made it — but nothing is on it. This applies the StorageClass, the validating
// admission policies, the namespaces, and the Argo Applications that let apl-core
// take over. Everything that follows in the catalog (`openbao-lifecycle` at
// provisioned, `identity-plane` and `harbor-provisioner` at seeded) depends on
// this having run.
//
// `secret-custody` at `provisioned` is legal by the row `cluster-access` earned,
// and true for the same reason: this places the GHCR pull secret and the instance
// repo's Argo credentials, which is a credential being put somewhere, not merely
// read.
//
// `read-repo` because the lane reads the assembled spec (clusterspec.Detected)
// to learn what it is bootstrapping.
//
// ONE EDGE COULD NOT BE CUT, and it is declared rather than hidden:
// PinnedTemplateRef is a package var defaulting to "". Its real implementation
// reads the copier answers file through the scaffold mass this campaign has
// recorded as blocked. The default returns empty rather than guessing — a wrong
// ref would be bootstrapped into a real cluster, and an empty one makes the caller
// say so.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "bootstrap-cluster",
		Short:  "make a bare LKE cluster ready for apl-core: storage class, admission policies, namespaces, Argo bridge",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			State: extension.Provisioned,
			Grants: []extension.Grant{
				extension.ReadRepo, extension.ClusterRead,
				extension.ClusterWrite, extension.SecretCustody,
			},
		}},
		Incomplete: []string{
			"PinnedTemplateRef is injected and defaults to \"\": its implementation " +
				"reads the copier answers file through `readAnswers`, which is inside " +
				"the blocked scaffold mass. package main installs the real one.",
		},
	}
}
