package seedspecial

// cobra_special.go — the CLI surface for special.
//
// Split from special.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func ResolveHarborURLCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "resolve-harbor-url",
		Short: "default HARBOR_URL to harbor.<domainSuffix> from the LandingZone spec",
		Long: "Native port of the 'Pre-flight — resolve Harbor URL for configuration'\n" +
			"step. HARBOR_URL is the registry hostname buildah pushes to / images pull\n" +
			"from (stored in OpenBao as registry_host) — NOT how the API is reached\n" +
			"(the in-cluster harbor-robot-provisioner talks to harbor-core.harbor.svc).\n" +
			"When the HARBOR_URL env (vars.HARBOR_URL) is set it wins; otherwise\n" +
			"harbor.<domainSuffix> is derived from the LandingZone spec\n" +
			"(spec.environments.<region>.cluster.bootstrap.domainSuffix — the host\n" +
			"apl-core already serves Harbor at) and written to $GITHUB_ENV. This used\n" +
			"to read cluster_domain from the rendered cluster-bootstrap tfvars; the\n" +
			"spec is mandatory now, so that tfvars side-channel (and the cluster_domain\n" +
			"variable it existed for) was retired. Fails only when neither is available.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunResolveHarborURL(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) whose domainSuffix derives the Harbor host (required)")
	return c
}
func AuditPVCStorageClassCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit-pvc-storageclass",
		Short: "warn about PVCs that escaped the Kyverno encrypted-StorageClass mutation",
		Long: "Native port of the 'Audit PVCs against encrypted-Retain StorageClass'\n" +
			"bootstrap step. Lists every PVC not on block-storage-retain as ::warning::\n" +
			"lines plus a step-summary block, SPLIT BY CAUSE. Two different things put a\n" +
			"PVC on an unencrypted Delete-reclaim class:\n" +
			"  • in gitea/istio-system, the Kyverno mutation covers the PVC but its\n" +
			"    webhook has a 30-90s readiness lag after CRD registration, so anything\n" +
			"    apl-core's helmfile created in that window escaped it;\n" +
			"  • anywhere else, Kyverno never applied — the chart honored\n" +
			"    cluster.defaultStorageClass, which defaults to '' (\"use the cluster\n" +
			"    default\"), so the PVC took whatever class was annotated default when\n" +
			"    apl-core created it. Widening the policy would not fix those.\n" +
			"Never fails the workflow — the cluster is functional, just less secure\n" +
			"than intended.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunAuditPVCStorageClass() },
	}
}
