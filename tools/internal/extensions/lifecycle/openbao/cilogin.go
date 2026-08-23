package openbao

// cilogin.go — `llz ci openbao-login`: obtain a short-lived OpenBao
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"

	"net/http"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// InClusterAddr is the ClusterIP address an in-cluster workload reaches
// OpenBao at — the same endpoint the reconciler and CronJobs use.
const InClusterAddr = "https://platform-llz-svc.cluster.local:8200"

func RunCILogin(dryRun bool, method, role, addr, mount, saTokenFile, exportVar string) error {
	if addr == "" {
		if addr = os.Getenv("OPENBAO_ADDR"); addr == "" {
			addr = InClusterAddr
		}
	}
	if dryRun {
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
	client, err := openbao.InClusterHTTPClient()
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
	// $GITHUB_ENV OR STDOUT, and the fallback is the whole point of the command.
	//
	// ghaout.Append is a SILENT NO-OP when its env var is unset — deliberately, so
	// the commands run from a workstation. That is right for a step summary and
	// wrong for a token: outside GitHub Actions this minted a real OpenBao token,
	// wrote it nowhere, printed "exported to $GITHUB_ENV" and exited 0. The caller
	// got no token and no error.
	//
	// AND OUTSIDE ACTIONS IS THE PRIMARY CASE. This file's own header argues that
	// `--method kubernetes` is the default because it "ties the job to NOTHING
	// GitHub-specific: it works from an Argo Workflow, a CronJob, the reconciler" —
	// none of which set $GITHUB_ENV. The one output channel was the one those
	// callers do not have.
	//
	// The fallback writes the BARE token to stdout so `T=$(llz ci openbao-login …)`
	// works, and does not mask on that path: ghaout.Mask writes `::add-mask::` to
	// STDOUT, which would land inside the capture. teamlogin.go records the same
	// trade for the same reason. Masking still happens on the $GITHUB_ENV path,
	// where stdout carries nothing.
	if os.Getenv("GITHUB_ENV") == "" {
		fmt.Fprintf(os.Stderr, "openbao-login: method=%s role=%s → token on stdout ($GITHUB_ENV is unset). "+
			"Capture it, e.g. %s=$(llz ci openbao-login …)\n", method, role, exportVar)
		fmt.Print(token) // stdout only, unmasked, so a capture gets the token and nothing else
		return nil
	}
	ghaout.MaskLines(token)
	if err := ghaout.Append("GITHUB_ENV", exportVar+"="+token); err != nil {
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
	oidcToken, err := forge.ActionsOIDCToken(forge.OIDCAudienceForRepo(ghRepo), nil)
	if err != nil {
		return "", fmt.Errorf("mint GitHub OIDC token: %w (does the job set `permissions: id-token: write`?)", err)
	}
	ghaout.MaskLines(oidcToken)
	return openbao.JWTLogin(ctx, client, addr, role, oidcToken)
}
