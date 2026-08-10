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
package reconcilelanes

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credpaths"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"

	"net/http"

	"fmt"
)

const (
	DefaultOpenBaoAddr    = openbao.DefaultAddr
	openbaoAuthMount      = "kubernetes"
	openbaoReconcilerRole = "reconciler"
)

// credpaths.CredPaths maps each OpenBao KV path whose rotation age we track to its metric
// `cred` label and its rotation class (above) — every platform credential that
// would otherwise age silently, which is exactly the blind spot the credential
// single pane exists to close. The one deliberate omission is the per-cluster
// Managed Postgres admin path, whose name is not known until a cluster is
// declared; it is enumerated at sample time instead (see credpaths.DBAdminRoot).
//
// Every path here MUST also be granted a `secret/metadata/<path>` read in
// policyReconcilerRead (ci_openbao_configure.go). A missing grant is a 403, and
// a 403 is a non-404 error that fails the WHOLE sampler pass (up=0) — it does
// not degrade to a single missing series.
// The `optional` column says whether the path is expected to EXIST on every
// deployment. It is orthogonal to class, which says what lowers the age once it
// does: `linode-cloud-firewall` is on-demand (a real 90d SLA, actionable by a
// human) AND opt-in (most instances never seed it). Conflating the two costs one
// property or the other — demote it to `static` and it silently loses the SLA the
// docs promise; leave it required and assert-rotation-health reds every stock
// cluster for a credential that is correctly absent.

// credpaths.DBAdminRoot is the KV collection holding one Managed Postgres admin credential
// per declared database cluster (`llz ci seed-db-admin` writes
// secret/infra/db-admin/<name>). The names come from each deployment's
// `databases` tfvar, so unlike every entry in credpaths.CredPaths they cannot be a literal
// here — the sampler LISTs the collection and tracks whatever it finds. That
// keeps a cluster added later covered with no code change, and a deployment with
// no databases simply lists nothing.
//
// Classified `on-demand`, not `static`: `llz ci rotate-db-admin` rotates these,
// so their age is something a human can act on.

// openbaoProbe is the slice of the OpenBao client the sampler needs.
type openbaoProbe interface {
	SealStatus(ctx context.Context) (openbao.SealInfo, error)
	MetadataUpdatedTime(ctx context.Context, path string) (time.Time, bool, error)
	MetadataList(ctx context.Context, path string) ([]string, bool, error)
}

// Seams for tests.
//
// Both build their transport from inClusterBaoHTTPClient, so this lane honours
// the same OPENBAO_CA_FILE / OPENBAO_SKIP_VERIFY contract as every other
// in-cluster OpenBao caller. It previously hardcoded HTTPClientInsecure, which
// meant mounting a CA into the reconciler could not have made its traffic
// verified — the manifest and the code disagreed silently.
// BaoHTTPClient builds the mTLS-aware transport every OpenBao call in this package
// goes through. It is a package var rather than a parameter because it is also the
// seam package main's tests already stub, and because `reconcile_apl_overlay.go` —
// which did NOT move, see the extension declaration — reaches OpenBao through the
// same three Fn vars below. Assigned once at wiring time by package main.
var BaoHTTPClient = func() (*http.Client, error) {
	return nil, fmt.Errorf("reconcilelanes: BaoHTTPClient was never wired — package main assigns it at startup")
}

var (
	// OpenBaoClientFn, OpenBaoLoginFn and OpenBaoJWTFn are exported because the
	// apl-overlay lane still lives in internal/cli and shares this credential path.
	// That sharing is the reason the two are one grant cluster and would move
	// together; it is recorded in the declaration rather than worked around.
	OpenBaoClientFn = func(addr, token string) (openbaoProbe, error) {
		hc, err := BaoHTTPClient()
		if err != nil {
			return nil, err
		}
		return openbao.NewWithClient(addr, token, "", hc), nil
	}
	OpenBaoLoginFn = func(ctx context.Context, addr, jwt string) (string, error) {
		hc, err := BaoHTTPClient()
		if err != nil {
			return "", err
		}
		return openbao.KubernetesLogin(ctx, hc, addr, openbaoAuthMount, openbaoReconcilerRole, jwt)
	}
	OpenBaoJWTFn = readServiceAccountToken
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

// SampleOpenBao publishes the seal + credential-age gauges. Seal is
// unauthenticated; the credential ages need a metadata-read token via k8s-auth.
// Any API/login failure returns an error (the manager records up=0); a 404 on a
// credential path (not seeded yet) is skipped, not an error.
func SampleOpenBao(ctx context.Context, reg *metrics.Registry, now time.Time) error {
	addr := os.Getenv("OPENBAO_ADDR")
	if addr == "" {
		addr = DefaultOpenBaoAddr
	}

	sealClient, err := OpenBaoClientFn(addr, "")
	if err != nil {
		return err
	}
	si, err := sealClient.SealStatus(ctx)
	if err != nil {
		return err
	}
	reg.SetGauge("llz_openbao_sealed", "1 if OpenBao is sealed", nil, BoolGauge(si.Sealed))
	reg.SetGauge("llz_openbao_initialized", "1 if OpenBao is initialized", nil, BoolGauge(si.Initialized))

	jwt, err := OpenBaoJWTFn()
	if err != nil {
		return err
	}
	tok, err := OpenBaoLoginFn(ctx, addr, jwt)
	if err != nil {
		return err
	}
	c, err := OpenBaoClientFn(addr, tok)
	if err != nil {
		return err
	}
	// Copied, not aliased: appending to a slice that shares credpaths.CredPaths' backing
	// array would let one sample pass scribble the next one's entries.
	paths := append([]credpaths.CredPath(nil), credpaths.CredPaths...)
	// Managed Postgres admin credentials, discovered rather than declared. A
	// deployment with no databases lists nothing (ok=false on the 404 KV v2
	// returns for an empty collection) and contributes no series.
	names, ok, err := c.MetadataList(ctx, credpaths.DBAdminRoot)
	if err != nil {
		return err
	}
	if ok {
		for _, n := range names {
			paths = append(paths, credpaths.CredPath{
				Path: credpaths.DBAdminRoot + "/" + n, Cred: "db-admin-" + n, Class: credpaths.CredClassOnDemand})
		}
	}

	for _, cp := range paths {
		t, ok, err := c.MetadataUpdatedTime(ctx, cp.Path)
		if err != nil {
			return err
		}
		if !ok {
			continue // not seeded yet
		}
		reg.SetGauge("llz_credential_age_days",
			"days since the credential was last rotated in OpenBao (Class: automated|on-demand|generate-once|tracks-source|static)",
			map[string]string{"cred": cp.Cred, "class": cp.Class}, float64(health.DaysSince(t, now)))
	}
	return nil
}

func BoolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
