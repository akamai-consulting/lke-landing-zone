package main

// openbao_k8s_login.go holds the one in-cluster OpenBao k8s-auth login shared by
// every workload that needs a write-capable client from inside the cluster.
//
// This existed twice, byte-identical apart from the default role — the second
// copy's own comment said so ("the same contract as the linode-cred-rotator's
// login"). Two copies of a TLS-posture decision is the kind of duplication worth
// collapsing on principle: the OPENBAO_CA_FILE / OPENBAO_SKIP_VERIFY branch
// decides whether pod→OpenBao traffic is verified, and a fix applied to one copy
// and not the other is a security difference nobody would see in review.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// openInClusterBaoStore logs in to OpenBao's kubernetes auth mount with the pod's
// ServiceAccount token and returns a write-capable client.
//
// defaultRole is the k8s-auth role to assume when OPENBAO_KUBERNETES_ROLE is
// unset — the only thing that ever differed between the callers.
func openInClusterBaoStore(ctx context.Context, defaultRole string) (baoStore, error) {
	addr := envOrDefault(os.Getenv, "OPENBAO_ADDR", "https://platform-openbao.llz-openbao.svc.cluster.local:8200")
	mount := envOrDefault(os.Getenv, "OPENBAO_KUBERNETES_MOUNT", "kubernetes")
	role := envOrDefault(os.Getenv, "OPENBAO_KUBERNETES_ROLE", defaultRole)
	saFile := envOrDefault(os.Getenv, "SA_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")

	// TLS to OpenBao: mount the CA and set OPENBAO_CA_FILE to verify it; otherwise
	// OPENBAO_SKIP_VERIFY=true falls back to the established in-cluster posture
	// (every baoExec uses VAULT_SKIP_VERIFY) for pod→OpenBao traffic. Neither set
	// is an error rather than a silent downgrade to unverified TLS.
	var httpClient *http.Client
	if caFile := os.Getenv("OPENBAO_CA_FILE"); caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read OPENBAO_CA_FILE: %w", err)
		}
		if httpClient, err = openbao.HTTPClientWithCA(caPEM, 30*time.Second); err != nil {
			return nil, err
		}
	} else if cli.EnvBool("OPENBAO_SKIP_VERIFY", false) {
		httpClient = openbao.HTTPClientInsecure(30 * time.Second)
	} else {
		return nil, fmt.Errorf("set OPENBAO_CA_FILE (mounted openbao CA) or OPENBAO_SKIP_VERIFY=true")
	}

	jwt, err := os.ReadFile(saFile)
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token: %w", err)
	}
	token, err := openbao.KubernetesLogin(ctx, httpClient, addr, mount, role, strings.TrimSpace(string(jwt)))
	if err != nil {
		return nil, err
	}
	return openbao.NewWithClient(addr, token, "", httpClient), nil
}
