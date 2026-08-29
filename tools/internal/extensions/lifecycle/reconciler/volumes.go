package reconciler

// volumes.go — the three storage commands, reduced to flag sets.
//
// Everything they do is `assert-storage` and lives in tools/internal/extensions/assertions/volumes. What
// stays here is the four capabilities that package is HANDED (volumes.Deps) and
// cannot reach for itself: the Linode token, the in-cluster client, the kubectl
// shell-out, and the GitHub step-summary sink.
//
// That list is the interesting output of this extraction. The three gates
// extracted before it needed no injection at all — they read files — which is why
// they were cheap and why they said nothing about extensions that touch a cluster
// or a cloud. Every high-coupling candidate in the catalog's closure census needs
// some subset of exactly these four, so this is the action ABI's requirements
// document, arrived at by extracting rather than by design.
//
// THE TWO CLUSTER PATHS ARE NOT INTERCHANGEABLE. The assert lane runs on a CI
// runner against a fetched kubeconfig and reads through kubectl; the two reconciler
// lanes run in-pod and read through the ServiceAccount client. Handing both to the
// package and letting each entry point take the one it needs keeps that distinction
// visible — collapsing them would make the assert lane fail in CI for a reason
// ("in-cluster config not found") that names none of its causes.

import (
	"context"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/volumes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// ciVolumeDeps is the CI-runner shape: kubectl for the cluster, and a step summary.
func ciVolumeDeps() volumes.Deps {
	return volumes.Deps{
		Token:   linode.InClusterLinodeToken(),
		Kubectl: func(args ...string) ([]byte, error) { return execOutput("kubectl", args...) },
		Summary: func(lines ...string) error { return ghaout.Append("GITHUB_STEP_SUMMARY", lines...) },
	}
}

// inClusterVolumeDeps is the in-pod shape: the ServiceAccount client, no kubectl.
func inClusterVolumeDeps() (volumes.Deps, error) {
	k, err := discoverKubeFn()
	if err != nil {
		return volumes.Deps{}, err
	}
	return volumes.Deps{Token: linode.InClusterLinodeToken(), Kube: k}, nil
}

func runCIReconcileVolumeTags(ctx context.Context, scName string) error {
	d, err := inClusterVolumeDeps()
	if err != nil {
		return err
	}
	return volumes.ReconcileTags(ctx, d, scName)
}
