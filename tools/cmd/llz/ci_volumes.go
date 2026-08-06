package main

// ci_volumes.go — the three storage commands, reduced to flag sets.
//
// Everything they do is `assert-storage` and lives in tools/internal/volumes. What
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

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/volumes"
)

// ciVolumeDeps is the CI-runner shape: kubectl for the cluster, and a step summary.
func ciVolumeDeps() volumes.Deps {
	return volumes.Deps{
		Token:   inclusterLinodeToken(),
		Kubectl: func(args ...string) ([]byte, error) { return execOutput("kubectl", args...) },
		Summary: func(lines ...string) error { return appendGHAFile("GITHUB_STEP_SUMMARY", lines...) },
	}
}

// inClusterVolumeDeps is the in-pod shape: the ServiceAccount client, no kubectl.
func inClusterVolumeDeps() (volumes.Deps, error) {
	k, err := discoverKubeFn()
	if err != nil {
		return volumes.Deps{}, err
	}
	return volumes.Deps{Token: inclusterLinodeToken(), Kube: k}, nil
}

func runCIReconcileVolumeTags(ctx context.Context, scName string) error {
	d, err := inClusterVolumeDeps()
	if err != nil {
		return err
	}
	return volumes.ReconcileTags(ctx, d, scName)
}

func runRelabelVolumes(ctx context.Context) error {
	d, err := inClusterVolumeDeps()
	if err != nil {
		return err
	}
	return volumes.Relabel(ctx, d)
}

func ciAssertVolumeEncryptionCmd() *cobra.Command {
	var scName string
	c := &cobra.Command{
		Use:   "assert-volume-encryption",
		Short: "FAIL if any PV-backed Linode Volume is unencrypted, untagged, or still named pvc-<uuid>",
		Long: "E2E gate for the storage invariant. Lists every Linode-CSI PV in the cluster,\n" +
			"GETs its backing Volume from the Linode API, and fails unless EVERY one is\n" +
			"encrypted, carries the tag set the StorageClass defines, and has been renamed\n" +
			"off the CSI default pvc-<uuid> to a readable <region>-<ns>-<pvc>.\n" +
			"\n" +
			"Tags and labels are applied by reconciler lanes after CreateVolume, so those two\n" +
			"get a bounded wait before failing. Encryption never does — it cannot change.\n" +
			"\n" +
			"Checks the Linode API rather than the PVC's storageClassName on purpose: the\n" +
			"class name is a proxy for encryption, and it was a configreadiness.Satisfied proxy the whole\n" +
			"time a managed cluster was provisioning unencrypted Volumes. `encryption` on\n" +
			"the Volume itself is the fact.\n" +
			"\n" +
			"Fails closed on every ambiguity, INCLUDING a cluster with no Linode-CSI PVs —\n" +
			"a gate that passes having examined nothing is worse than no gate.\n" +
			"\n" +
			"A color.Red run is not re-runnable: encryption is set inside CreateVolume and\n" +
			"storageClassName is immutable once bound, so the fix is to re-roll the workload\n" +
			"onto a class that encrypts (which destroys that volume's data).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return volumes.AssertEncryption(cmd.Context(), ciVolumeDeps(), scName)
		},
	}
	c.Flags().StringVar(&scName, "storage-class", volumes.DefaultTagsSC,
		"StorageClass whose volumeTags parameter defines the required tag set")
	return c
}

func ciReconcileVolumeTagsCmd() *cobra.Command {
	var scName string
	c := &cobra.Command{
		Use:   "reconcile-volume-tags",
		Short: "heal StorageClass volumeTags onto every PV-backed Linode Volume",
		Long: "One-shot tag reconciler (also the llz-reconciler's `volume-tags` lane): reads\n" +
			"the desired tag set from the StorageClass's linodebs.csi.linode.com/volumeTags\n" +
			"parameter, lists the cluster's Linode-CSI PVs, and PUTs any missing tags onto\n" +
			"their backing Volumes (labels untouched — the volume-labels lane owns those).\n" +
			"Exists for Volumes born untagged — e.g. a clone/snapshot PVC admitted while\n" +
			"admission control was degraded (the Linode clone API takes no tags). Also\n" +
			"reports (never deletes) this cluster's abandoned Retain Volumes: tagged\n" +
			"lke<id> but referenced by no PV. Reads LINODE_TOKEN (env or the optional\n" +
			"linode-api-token Secret volume) and the cluster's PVs + StorageClass through\n" +
			"the in-pod ServiceAccount. Idempotent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCIReconcileVolumeTags(cmd.Context(), scName)
		},
	}
	c.Flags().StringVar(&scName, "storage-class", volumes.DefaultTagsSC, "StorageClass whose volumeTags parameter defines the desired tag set")
	return c
}

func ciRelabelVolumesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "relabel-volumes",
		Short: "rename Linode Volumes to <region>-<ns>-<pvc> for every bound Linode-CSI PV",
		Long: "In-cluster Linode Volume relabeler — the Go port of the linode-volume-labeler\n" +
			"relabel.sh CronJob. Lists cluster PVs and, for each bound Linode-CSI volume,\n" +
			"rewrites its Linode UI label from the CSI default pvc-<uuid> to a readable\n" +
			"<REGION_SHORT>-<namespace>-<pvc-name> (sanitized to Linode's charset, truncated\n" +
			"to 32 chars). Idempotent — already-correct labels are skipped; a volume deleted\n" +
			"out-of-band (absent from the account list) is skipped. Env: REGION_SHORT,\n" +
			"LINODE_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runRelabelVolumes(context.Background()) },
	}
}
