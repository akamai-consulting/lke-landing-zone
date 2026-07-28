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

// Rotation classes for the credential-age gauge. The class is what keeps
// LLZCredentialRotationOverdue honest: only `automated` paths have a rotator
// behind them, so only they can be EXPECTED to reset their age. Publishing the
// other two classes without the label would mean an alert that fires on day 91
// and can never be cleared by anything except a human doing a manual re-seed —
// i.e. permanent noise that trains operators to ignore the rule.
const (
	// credClassAutomated — a rotator resets this path on a cadence
	// (linodeCredRotator, ~80d threshold). Eligible for the 90d SLA alert.
	credClassAutomated = "automated"
	// credClassGenerateOnce — created in-cluster by an ESO PushSecret with a
	// Password generator and `updatePolicy: IfNotExists`, so it is written once
	// and never again. Its age is REAL (and worth seeing on the dashboard), but
	// no automation will ever lower it. Visible, not alertable.
	credClassGenerateOnce = "generate-once"
	// credClassTracksSource — mirrored from a source of truth outside OpenBao
	// (harbor/admin follows Harbor's Helm-generated Secret via a `Replace`
	// PushSecret). Age tracks the SOURCE's rotation, not OpenBao's, so an
	// overdue reading is a statement about Harbor, not about the vault.
	credClassTracksSource = "tracks-source"
	// credClassOnDemand — a real rotation path exists, but an OPERATOR triggers
	// it rather than a schedule: the Managed Postgres admin credential, rotated
	// by `llz ci rotate-db-admin` (secret-rotation.yml scope=db-admin). Linode
	// resets that password in place with no overlap window, so every consumer
	// breaks until ESO re-syncs — which is why it is deliberately not on a cron
	// (docs/designs/shared-managed-postgres.md).
	//
	// It shares the 90d SLA alert with `automated` because the age IS
	// actionable — someone can dispatch the workflow — which is the whole test
	// for whether an alert should fire. Classing it `static` would have given
	// the highest-value credential in the deployment a yearly INFO nudge and
	// exempted it from the only rule that asks a human to do something.
	//
	// It is not `automated` either: there, a 90d breach means the ROTATOR is
	// broken; here it means nobody has run it. Different remedy, so a different
	// label — the alert description branches on it.
	credClassOnDemand = "on-demand"
	// credClassStatic — seeded once by bootstrap (or by an operator) and never
	// touched again by any automation: the Harbor robots, the two GitHub-PAT
	// copies, the Slack webhook. This is the `static` row docs/secrets.md's
	// rotation legend has always had and the metric taxonomy did not — so these
	// paths published NO series at all and were invisible on the single pane
	// rather than visibly old. Re-seeding by hand is the only thing that lowers
	// the age, so like the two classes above they get the yearly info nudge,
	// never the 90d SLA.
	credClassStatic = "static"
)

// credPaths maps each OpenBao KV path whose rotation age we track to its metric
// `cred` label and its rotation class (above) — every platform credential that
// would otherwise age silently, which is exactly the blind spot the credential
// single pane exists to close. The one deliberate omission is the per-cluster
// Managed Postgres admin path, whose name is not known until a cluster is
// declared; it is enumerated at sample time instead (see dbAdminRoot).
//
// Every path here MUST also be granted a `secret/metadata/<path>` read in
// policyReconcilerRead (ci_openbao_configure.go). A missing grant is a 403, and
// a 403 is a non-404 error that fails the WHOLE sampler pass (up=0) — it does
// not degrade to a single missing series.
var credPaths = []struct{ path, cred, class string }{
	{"secret/loki/object-store", "loki-object-store", credClassAutomated},
	{"secret/harbor/registry-s3", "harbor-registry-s3", credClassAutomated},
	{"secret/obj/platform", "obj-platform", credClassAutomated},
	// The narrow in-cluster PAT. Re-minted monthly per region by
	// secret-rotation.yml → `rotate-incluster-pat`, so it is genuinely
	// `automated` and belongs on the 90d SLA: a stalled rotation workflow is
	// exactly the failure this alert can ask a human to fix. It was the one
	// credential with real rotation and no watchdog over it.
	{"secret/linode/api-token", "linode-incluster-pat", credClassAutomated},
	// The BROAD account read_write PAT — the highest-privilege Linode credential
	// the platform holds. The broadPatRotator CronJob re-mints it weekly against
	// ROTATE_AFTER_DAYS, so it is `automated` and belongs on the 90d SLA for the
	// same reason the narrow PAT does. Its EXPIRY already rode in free via the
	// token-inventory's Linode enumeration; what was missing is rotation age —
	// so a wedged rotator stayed invisible until the token actually lapsed.
	{"secret/linode/broad-pat", "linode-broad-pat", credClassAutomated},
	{"secret/grafana/admin", "grafana-admin", credClassGenerateOnce},
	{"secret/otel/ingress", "otel-ingress", credClassGenerateOnce},
	{"secret/harbor/admin", "harbor-admin", credClassTracksSource},
	// Bootstrap seeds, re-seeded only by hand. `secret/alerts/webhooks` is
	// operator-seeded and only when spec.alerting.receivers includes slack; an
	// instance without it 404s and is skipped, same as any unseeded path.
	{"secret/harbor/robot", "harbor-robot", credClassStatic},
	{"secret/harbor/pull-robot", "harbor-pull-robot", credClassStatic},
	{"secret/cert-automation/github-token", "cert-automation-github-token", credClassStatic},
	{"secret/infra/github-dispatch-token", "infra-github-dispatch-token", credClassStatic},
	// The third copy of a GitHub PAT held in OpenBao, and the same drift signal as
	// the two above: token-inventory measures the GitHub-side expiry of
	// APL_VALUES_REPO_TOKEN, but nothing re-seeds THIS copy when an operator
	// rotates that PAT, and a stale copy is what actually breaks apl-core's
	// otomi.git and the argocd repo Secrets.
	{"secret/infra/apl-values-repo-token", "infra-apl-values-repo-token", credClassStatic},
	{"secret/alerts/webhooks", "alerts-webhooks", credClassStatic},
	// OPT-IN least-privilege firewall token (docs/consume-lke-landing-zone-internal.md).
	// Most instances never seed it — the sampler 404s and publishes nothing, same
	// as any unseeded path. Where it IS seeded it is operator-managed on a
	// documented ≤90d policy, so `on-demand` (actionable, 90d SLA) rather than
	// `static` (yearly nudge) is what matches the posture the docs promise.
	{"secret/linode/cloud-firewall", "linode-cloud-firewall", credClassOnDemand},
}

// dbAdminRoot is the KV collection holding one Managed Postgres admin credential
// per declared database cluster (`llz ci seed-db-admin` writes
// secret/infra/db-admin/<name>). The names come from each deployment's
// `databases` tfvar, so unlike every entry in credPaths they cannot be a literal
// here — the sampler LISTs the collection and tracks whatever it finds. That
// keeps a cluster added later covered with no code change, and a deployment with
// no databases simply lists nothing.
//
// Classified `on-demand`, not `static`: `llz ci rotate-db-admin` rotates these,
// so their age is something a human can act on.
const dbAdminRoot = "secret/infra/db-admin"

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
var (
	openbaoClientFn = func(addr, token string) (openbaoProbe, error) {
		hc, err := inClusterBaoHTTPClient()
		if err != nil {
			return nil, err
		}
		return openbao.NewWithClient(addr, token, "", hc), nil
	}
	openbaoLoginFn = func(ctx context.Context, addr, jwt string) (string, error) {
		hc, err := inClusterBaoHTTPClient()
		if err != nil {
			return "", err
		}
		return openbao.KubernetesLogin(ctx, hc, addr, openbaoAuthMount, openbaoReconcilerRole, jwt)
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

	sealClient, err := openbaoClientFn(addr, "")
	if err != nil {
		return err
	}
	si, err := sealClient.SealStatus(ctx)
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
	c, err := openbaoClientFn(addr, tok)
	if err != nil {
		return err
	}
	// Copied, not aliased: appending to a slice that shares credPaths' backing
	// array would let one sample pass scribble the next one's entries.
	paths := append([]struct{ path, cred, class string }(nil), credPaths...)
	// Managed Postgres admin credentials, discovered rather than declared. A
	// deployment with no databases lists nothing (ok=false on the 404 KV v2
	// returns for an empty collection) and contributes no series.
	names, ok, err := c.MetadataList(ctx, dbAdminRoot)
	if err != nil {
		return err
	}
	if ok {
		for _, n := range names {
			paths = append(paths, struct{ path, cred, class string }{
				dbAdminRoot + "/" + n, "db-admin-" + n, credClassOnDemand})
		}
	}

	for _, cp := range paths {
		t, ok, err := c.MetadataUpdatedTime(ctx, cp.path)
		if err != nil {
			return err
		}
		if !ok {
			continue // not seeded yet
		}
		reg.SetGauge("llz_credential_age_days",
			"days since the credential was last rotated in OpenBao (class: automated|on-demand|generate-once|tracks-source|static)",
			map[string]string{"cred": cp.cred, "class": cp.class}, float64(health.DaysSince(t, now)))
	}
	return nil
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
