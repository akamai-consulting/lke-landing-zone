package main

// ci_openbao_login.go — `llz ci openbao-login`: obtain a short-lived OpenBao
// token and export it to $GITHUB_ENV (or print it) for later steps. The auth
// primitive behind the CI-agnostic day-2 pattern (docs/designs/cross-org-reuse-pattern.md).
//
// Two methods, and the DEFAULT is deliberately the CI-agnostic one:
//
//   --method kubernetes (default) — the pod's ServiceAccount token → OpenBao's
//     `kubernetes` auth method. This ties the job to NOTHING GitHub-specific: it
//     works from an Argo Workflow, a CronJob, the reconciler, or any in-cluster
//     runner. It is the same path the reconciler / harbor-provisioner /
//     linode-cred-rotator already use. This is the direction for abstracting the
//     CI/CD pipeline — auth is workload identity, not a CI vendor's token.
//
//   --method oidc — a GitHub Actions OIDC token → OpenBao's `jwt` auth method.
//     Needs `permissions: id-token: write`. GitHub-coupled by construction, so it
//     is opt-in, not the default.
//
//     NO LONGER a fallback for a genuinely external GitHub-hosted caller, which
//     is what it was introduced as. OpenBao's listener now requires a client
//     certificate (ADR 0010), and an external runner has neither an
//     llz-client-ca identity nor in-cluster DNS for the ClusterIP — so this
//     method, like --method kubernetes, only works from a pod that mounts the
//     mTLS material. What changed is the TRANSPORT requirement, not the auth
//     method: the OIDC token is still what proves identity to the `jwt` mount.
//     External access goes through `kubectl port-forward … :8210` (the loopback
//     listener), which is deliberately exempt from the client-cert requirement.
//     No workflow in this repo currently invokes either method.
//
// Either way the OpenBao role's bound_service_account/bound_claims pin it to this
// cluster/repo (llz ci bao-configure), so a token from elsewhere can't use it.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"net/http"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
	"github.com/spf13/cobra"
)

// inClusterOpenBaoAddr is the ClusterIP address an in-cluster workload reaches
// OpenBao at — the same endpoint the reconciler and CronJobs use.
const inClusterOpenBaoAddr = "https://platform-openbao.llz-openbao.svc.cluster.local:8200"

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
			return runOpenBaoLogin(gopts, method, role, addr, mount, saTokenFile, exportVar)
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

func runOpenBaoLogin(g globalOpts, method, role, addr, mount, saTokenFile, exportVar string) error {
	if addr == "" {
		if addr = os.Getenv("OPENBAO_ADDR"); addr == "" {
			addr = inClusterOpenBaoAddr
		}
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) openbao-login method=%s role=%s addr=%s export=%s\n", method, role, addr, exportVar)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// This command runs as an in-cluster step (Argo/CI pod) and reaches OpenBao
	// on the ClusterIP, so it is pod-network traffic and needs the workload's
	// client certificate — the same identity the reconciler and CronJobs mount.
	// A step that runs somewhere without one must go through the loopback
	// listener instead (`kubectl port-forward … :8210`), not fall back to
	// unverified TLS.
	client, err := inClusterBaoHTTPClient()
	if err != nil {
		return err
	}

	var token string
	switch method {
	case "kubernetes", "":
		if role == "" {
			role = "reconciler"
		}
		token, err = kubernetesOpenBaoLogin(ctx, client, addr, mount, role, saTokenFile)
	case "oidc":
		if role == "" {
			role = "platform-ci"
		}
		token, err = oidcOpenBaoLogin(ctx, client, addr, role)
	default:
		return fmt.Errorf("unknown --method %q (want kubernetes|oidc)", method)
	}
	if err != nil {
		return err
	}
	maskGHALines(token)
	if err := appendGHAFile("GITHUB_ENV", exportVar+"="+token); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "openbao-login: method=%s role=%s → %s exported to $GITHUB_ENV (masked)\n", method, role, exportVar)
	return nil
}

// kubernetesOpenBaoLogin reads the pod's ServiceAccount token and exchanges it at
// OpenBao's kubernetes auth method — the CI-agnostic in-cluster path.
func kubernetesOpenBaoLogin(ctx context.Context, client *http.Client, addr, mount, role, saTokenFile string) (string, error) {
	if mount == "" {
		mount = cigate.EnvOr("OPENBAO_KUBERNETES_MOUNT", "kubernetes")
	}
	if saTokenFile == "" {
		saTokenFile = cigate.EnvOr("SA_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")
	}
	jwt, err := os.ReadFile(saTokenFile)
	if err != nil {
		return "", fmt.Errorf("read ServiceAccount token %s: %w (is this running in-cluster?)", saTokenFile, err)
	}
	return openbao.KubernetesLogin(ctx, client, addr, mount, role, strings.TrimSpace(string(jwt)))
}

// oidcOpenBaoLogin mints a GitHub Actions OIDC token and exchanges it at OpenBao's
// jwt auth method — the fallback for an external GitHub-hosted caller.
func oidcOpenBaoLogin(ctx context.Context, client *http.Client, addr, role string) (string, error) {
	ghRepo := os.Getenv("GITHUB_REPOSITORY")
	if ghRepo == "" {
		return "", fmt.Errorf("GITHUB_REPOSITORY is empty — cannot derive the OIDC audience for the %s jwt login", role)
	}
	oidcToken, err := githubActionsOIDCToken(oidcAudienceForRepo(ghRepo), nil)
	if err != nil {
		return "", fmt.Errorf("mint GitHub OIDC token: %w (does the job set `permissions: id-token: write`?)", err)
	}
	maskGHALines(oidcToken)
	return openbao.JWTLogin(ctx, client, addr, role, oidcToken)
}
