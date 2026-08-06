package main

// ci_kyverno.go — the `llz ci apply-kyverno-policy` flag set.
//
// The apply is tools/internal/kyverno, which declares the extension. `warn` is
// NOT re-exported: it is a two-line ::warning:: printer that happened to be
// defined in this file and is used by two other package main files, so main keeps
// its own and the package keeps its own. Two copies of a printf beat one exported
// symbol whose only job is to be reachable from both sides.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kyverno"
	"github.com/spf13/cobra"
)

func ciApplyKyvernoPolicyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply-kyverno-policy",
		Short: "apply a Kyverno ClusterPolicy with readiness poll + webhook-race soft-fail (terraform local-exec body)",
		Long: "Shared local-exec body for the kyverno_* null_resources in cluster-bootstrap.\n" +
			"Reads its config from the same environment variables those null_resources set\n" +
			"(KUBECONFIG_RAW, POLICY_MANIFEST, WAIT_FOR_KYVERNO, WAIT_TIMEOUT_SECONDS,\n" +
			"FIELD_MANAGER, {TIMEOUT,CRD_MISSING,WEBHOOK_RACE}_WARNING, RETROFIT_*), polls\n" +
			"until Kyverno can admit the policy, server-side-applies it, soft-fails on the\n" +
			"kyverno-svc admission-webhook race, and runs the optional retrofit kick.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return kyverno.Run() },
	}
}
