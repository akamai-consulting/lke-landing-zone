package main

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
	"github.com/spf13/cobra"
)

// ci_openbao_login_cmd.go — the flag set for `llz ci openbao-login`. The lane is
// internal/openbao.

func ciOpenBaoLoginCmd() *cobra.Command {
	var method, role, addr, mount, saTokenFile, exportVar string
	c := &cobra.Command{
		Use:   "openbao-login",
		Short: "obtain an OpenBao token via ServiceAccount (default) or GitHub OIDC and export it",
		Long: "Logs in to OpenBao and writes the resulting short-lived token to $GITHUB_ENV\n" +
			"as OPENBAO_TOKEN (override with --export-var), masked. The CI-agnostic auth\n" +
			"primitive for in-cluster day-2 work (docs/designs/cross-org-reuse-pattern.md).\n\n" +
			"--method kubernetes (default): the pod ServiceAccount token → OpenBao's\n" +
			"kubernetes auth — works from any in-cluster workload, nothing GitHub-specific.\n" +
			"--method oidc: a GitHub Actions OIDC token → OpenBao's jwt auth (needs\n" +
			"`permissions: id-token: write`).\n\n" +
			"BOTH methods now require this to run IN-CLUSTER with a client certificate:\n" +
			"OpenBao's listener verifies client certs, so the caller must mount an\n" +
			"llz-client-ca identity (OPENBAO_CLIENT_CERT_FILE / _KEY_FILE) plus the\n" +
			"openbao-ca anchor (OPENBAO_CA_FILE). --method oidc was previously described\n" +
			"as the fallback for an EXTERNAL GitHub-hosted caller; that is no longer\n" +
			"true — an external runner has neither the certificate nor in-cluster DNS\n" +
			"for the ClusterIP. Reach OpenBao from outside via\n" +
			"`kubectl port-forward … :8210` (the loopback listener) instead.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return openbao.RunCILogin(gopts.dryRun, method, role, addr, mount, saTokenFile, exportVar)
		},
	}
	c.Flags().StringVar(&method, "method", "kubernetes", "auth method: kubernetes (ServiceAccount, default) | oidc (GitHub OIDC)")
	c.Flags().StringVar(&role, "role", "", "OpenBao role (default: reconciler for kubernetes, platform-ci for oidc)")
	c.Flags().StringVar(&addr, "addr", "", "OpenBao API address (default: $OPENBAO_ADDR, else the in-cluster ClusterIP)")
	c.Flags().StringVar(&mount, "kubernetes-mount", "", "kubernetes auth mount path (default: $OPENBAO_KUBERNETES_MOUNT, else kubernetes)")
	c.Flags().StringVar(&saTokenFile, "sa-token-file", "", "ServiceAccount token file for --method kubernetes (default: $SA_TOKEN_FILE, else the projected SA token)")
	c.Flags().StringVar(&exportVar, "export-var", "OPENBAO_TOKEN", "$GITHUB_ENV variable to export the token as")
	return c
}
