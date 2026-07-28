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
	addr := envOr("OPENBAO_ADDR", "https://platform-openbao.llz-openbao.svc.cluster.local:8200")
	mount := envOr("OPENBAO_KUBERNETES_MOUNT", "kubernetes")
	role := envOr("OPENBAO_KUBERNETES_ROLE", defaultRole)
	saFile := envOr("SA_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")

	httpClient, err := inClusterBaoHTTPClient()
	if err != nil {
		return nil, err
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

// inClusterBaoHTTPClient builds the pod→OpenBao transport from the TLS posture
// the workload's manifest declares. It is the ONE place that decision lives.
//
// Mount the openbao CA and point OPENBAO_CA_FILE at it to verify OpenBao's
// serving cert (the cert-manager `openbao-ca` ClusterIssuer signs it; each
// consumer namespace issues its own bundle from that issuer — see
// platform-apl/components/openbaoCABundle/). OPENBAO_SKIP_VERIFY=true is the
// explicit opt-out. Setting NEITHER is an ERROR, deliberately: an unset
// environment must not silently mean unverified TLS, which is how the skip
// became universal in the first place.
//
// Extracted from openInClusterBaoStore so the reconciler's sampler lanes stop
// hardcoding HTTPClientInsecure. Those lanes had no CA path at all, so mounting
// a CA could not have changed their behaviour — the manifest would say
// "verified" while the code ignored it.
func inClusterBaoHTTPClient() (*http.Client, error) {
	if caFile := os.Getenv("OPENBAO_CA_FILE"); caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read OPENBAO_CA_FILE: %w", err)
		}
		return openbao.HTTPClientWithCA(caPEM, 30*time.Second)
	}
	if cli.EnvBool("OPENBAO_SKIP_VERIFY", false) {
		return openbao.HTTPClientInsecure(30 * time.Second), nil
	}
	return nil, fmt.Errorf("set OPENBAO_CA_FILE (the mounted openbao CA bundle) or OPENBAO_SKIP_VERIFY=true")
}
