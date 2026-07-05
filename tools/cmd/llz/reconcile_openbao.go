// The OpenBao gauges — the opt-in, read-only reconciler that surfaces OpenBao's
// seal state and credential rotation age (see docs/designs/kube-native-reconciler.md).
//
// Unlike the observe reconciler (kube-API only), these reads need OpenBao network
// egress + Kubernetes-auth, which is per-env wiring — so this is OFF by default
// behind --reconcile-openbao-gauges. It is read-only (no leader gate). Seal is the
// unauthenticated /v1/sys/seal-status; the credential ages read only KV-v2
// metadata (updated_time) via the reconciler's k8s-auth role — no access to the
// secret data itself. Retires the daily openbao-health seal check and the
// loki-objkey rotation-SLA check.
package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

const (
	defaultOpenBaoAddr    = "https://platform-openbao.llz-openbao.svc.cluster.local:8200"
	openbaoAuthMount      = "kubernetes"
	openbaoReconcilerRole = "reconciler"
)

// credPaths maps each OpenBao KV path whose rotation age we track to its metric
// `cred` label. These are the in-cluster-rotated object-storage keys.
var credPaths = []struct{ path, cred string }{
	{"secret/loki/object-store", "loki-object-store"},
	{"secret/harbor/registry-s3", "harbor-registry-s3"},
}

// openbaoProbe is the slice of the OpenBao client the sampler needs.
type openbaoProbe interface {
	SealStatus(ctx context.Context) (openbao.SealInfo, error)
	MetadataUpdatedTime(ctx context.Context, path string) (time.Time, bool, error)
}

// Seams for tests.
var (
	openbaoClientFn = func(addr, token string) openbaoProbe {
		return openbao.NewWithClient(addr, token, "", openbao.HTTPClientInsecure(30*time.Second))
	}
	openbaoLoginFn = func(ctx context.Context, addr, jwt string) (string, error) {
		return openbao.KubernetesLogin(ctx, openbao.HTTPClientInsecure(30*time.Second),
			addr, openbaoAuthMount, openbaoReconcilerRole, jwt)
	}
	openbaoJWTFn = readServiceAccountToken
)

func readServiceAccountToken() (string, error) {
	f := os.Getenv("SA_TOKEN_FILE")
	if f == "" {
		f = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// sampleOpenBao publishes the seal + credential-age gauges. Seal is
// unauthenticated; the credential ages need a metadata-read token via k8s-auth.
// Any API/login failure returns an error (the manager records up=0); a 404 on a
// credential path (not seeded yet) is skipped, not an error.
func sampleOpenBao(ctx context.Context, reg *metrics.Registry, now time.Time) error {
	addr := os.Getenv("OPENBAO_ADDR")
	if addr == "" {
		addr = defaultOpenBaoAddr
	}

	si, err := openbaoClientFn(addr, "").SealStatus(ctx)
	if err != nil {
		return err
	}
	reg.SetGauge("llz_openbao_sealed", "1 if OpenBao is sealed", nil, boolGauge(si.Sealed))
	reg.SetGauge("llz_openbao_initialized", "1 if OpenBao is initialized", nil, boolGauge(si.Initialized))

	jwt, err := openbaoJWTFn()
	if err != nil {
		return err
	}
	tok, err := openbaoLoginFn(ctx, addr, jwt)
	if err != nil {
		return err
	}
	c := openbaoClientFn(addr, tok)
	for _, cp := range credPaths {
		t, ok, err := c.MetadataUpdatedTime(ctx, cp.path)
		if err != nil {
			return err
		}
		if !ok {
			continue // not seeded yet
		}
		reg.SetGauge("llz_credential_age_days",
			"days since the credential was last rotated in OpenBao",
			map[string]string{"cred": cp.cred}, float64(health.DaysSince(t, now)))
	}
	return nil
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
