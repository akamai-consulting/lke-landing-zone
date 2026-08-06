package main

// openbao_k8s_login.go holds the one in-cluster OpenBao k8s-auth login shared by
// every workload that needs a write-capable client from inside the cluster, plus
// the single place that decides the pod→OpenBao TLS posture.
//
// This existed twice, byte-identical apart from the default role — the second
// copy's own comment said so ("the same contract as the linode-cred-rotator's
// login"). Two copies of a TLS-posture decision is the kind of duplication worth
// collapsing on principle: the branch below decides whether pod→OpenBao traffic
// is mutually authenticated, and a fix applied to one copy and not the other is
// a security difference nobody would see in review. Every pod-network caller now
// routes through inClusterBaoHTTPClient for that reason.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reconcilelanes"
)

// Default mount paths for the client identity every in-cluster workload projects
// from its `llz-client-ca`-issued Certificate Secret, and for the OpenBao serving
// CA it verifies the server against.
//
// These are defaults, not policy: each workload's manifest may override via env.
// They match the volumeMounts in components/{llzReconciler,harbor,broadPatRotator}.
const (
	defaultOpenBaoCAFile     = "/etc/openbao-ca/ca.crt"
	defaultOpenBaoClientCert = "/etc/openbao-client-tls/tls.crt"
	defaultOpenBaoClientKey  = "/etc/openbao-client-tls/tls.key"
)

// inClusterBaoHTTPClient builds the mutually-authenticated transport for
// pod→OpenBao traffic and memoizes it, so a command that logs in and then makes
// many calls reuses one connection pool (and reads the keypair off disk once).
//
// THE POSTURE, in one place: OpenBao's listener sets
// tls_require_and_verify_client_cert, so there is no unauthenticated path to it
// over the pod network any more. A caller either has a client keypair issued by
// llz-client-ca or it cannot connect. The old OPENBAO_SKIP_VERIFY escape hatch
// is gone deliberately — it degraded silently to unverified TLS, and every
// workload that set it now mounts a real identity instead.
//
// Errors are explicit about WHICH file is missing: a cert-less pod otherwise
// fails deep in the TLS handshake with "remote error: tls: bad certificate",
// which reads like a server problem.
var inClusterBaoHTTPClient = newInClusterBaoHTTPClient()

// newInClusterBaoHTTPClient returns the memoizing constructor above.
//
// It caches SUCCESS ONLY, and deliberately not with sync.OnceValues. The CA
// bundle volume is `optional` on the reconciler (#358's posture — the pod's
// many non-OpenBao lanes must not wait on a wave-0 Certificate), so on a cold
// start the file can be briefly absent. OnceValues would cache that failure for
// the life of the process, and the reconciler is a long-lived Deployment whose
// liveness probe never touches OpenBao — it would never recover, and the cure
// would be a manual restart nobody knows to perform.
//
// Retrying on failure costs one file read per OpenBao pass while the material
// is missing, and nothing once it lands. Split out as a constructor so a test
// can reset the cache.
func newInClusterBaoHTTPClient() func() (*http.Client, error) {
	var (
		mu     sync.Mutex
		cached *http.Client
	)
	return func() (*http.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil {
			return cached, nil
		}
		c, err := buildInClusterBaoHTTPClient()
		if err != nil {
			return nil, err
		}
		cached = c
		return c, nil
	}
}

func buildInClusterBaoHTTPClient() (*http.Client, error) {
	caFile := cigate.EnvOr("OPENBAO_CA_FILE", defaultOpenBaoCAFile)
	certFile := cigate.EnvOr("OPENBAO_CLIENT_CERT_FILE", defaultOpenBaoClientCert)
	keyFile := cigate.EnvOr("OPENBAO_CLIENT_KEY_FILE", defaultOpenBaoClientKey)

	// FromFiles, not the byte-slice form: this client is memoized for the life of
	// the process, and the reconciler's process outlives a 90-day certificate. A
	// client that captured the keypair at startup would keep presenting the
	// expired leaf after cert-manager renewed it — and since the liveness probe
	// never touches OpenBao, nothing would restart the pod. FromFiles re-reads
	// per handshake so a renewal is picked up without a restart.
	c, err := openbao.HTTPClientMTLSFromFiles(caFile, certFile, keyFile, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w — this workload needs a Certificate issued by the llz-client-ca ClusterIssuer and the openbao-ca anchor mounted; see docs/adr/0010-in-cluster-mtls.md", err)
	}
	return c, nil
}

// openInClusterBaoStore logs in to OpenBao's kubernetes auth mount with the pod's
// ServiceAccount token and returns a write-capable client.
//
// defaultRole is the k8s-auth role to assume when OPENBAO_KUBERNETES_ROLE is
// unset — the only thing that ever differed between the callers.
//
// Two credentials are in play and they authenticate different things: the client
// CERTIFICATE proves at the transport layer that this pod is an LLZ workload
// (and is what the listener gates on), while the ServiceAccount TOKEN proves to
// the kubernetes auth mount which role it may assume. Neither replaces the
// other — the cert says "you may speak to me", the token says "and this is what
// you may read".
func openInClusterBaoStore(ctx context.Context, defaultRole string) (baoStore, error) {
	addr := cigate.EnvOr("OPENBAO_ADDR", reconcilelanes.DefaultOpenBaoAddr)
	mount := cigate.EnvOr("OPENBAO_KUBERNETES_MOUNT", "kubernetes")
	role := cigate.EnvOr("OPENBAO_KUBERNETES_ROLE", defaultRole)
	saFile := cigate.EnvOr("SA_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")

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
