// Package credpaths is the registry of OpenBao KV paths this platform keeps
// credentials at, and how each one is expected to be rotated.
//
// IT IS A TABLE, NOT A CAPABILITY, and it came out of
// internal/extensions/reconcilelanes where it was neither. That package was
// imported by five peers -- assert-secrets, bootstrap-cluster, database,
// reconciler, token-inventory -- and owned no cobra command at all. What the five
// wanted was this: CredPaths (35 references), the five rotation classes (41), and
// DBAdminRoot (8). None of them wanted the credential-age GAUGE, which is what
// reconcile-lanes is actually for.
//
// THE CLASS IS THE PART WITH A RULE IN IT, and it is why this is worth its own
// package rather than a slice of strings. Only `automated` paths have a rotator
// behind them, so only they can be EXPECTED to reset their age. Publishing the
// other four classes without the label would mean an alert that fires on day 91
// and can never be cleared by anything except a human doing a manual re-seed --
// permanent noise that trains operators to ignore the rule. Every consumer of this
// table has to make that distinction, which is precisely why the table and the
// classes could not be separated from each other and had to leave together.
package credpaths

// Rotation classes for the credential-age gauge. The class is what keeps
// LLZCredentialRotationOverdue honest: only `automated` paths have a rotator
// behind them, so only they can be EXPECTED to reset their age. Publishing the
// other two classes without the label would mean an alert that fires on day 91
// and can never be cleared by anything except a human doing a manual re-seed —
// i.e. permanent noise that trains operators to ignore the rule.
const (
	// CredClassAutomated — a rotator resets this path on a cadence
	// (linodeCredRotator, ~80d threshold). Eligible for the 90d SLA alert.
	CredClassAutomated = "automated"
	// CredClassGenerateOnce — created in-cluster by an ESO PushSecret with a
	// Password generator and `updatePolicy: IfNotExists`, so it is written once
	// and never again. Its age is REAL (and worth seeing on the dashboard), but
	// no automation will ever lower it. Visible, not alertable.
	CredClassGenerateOnce = "generate-once"
	// CredClassTracksSource — mirrored from a source of truth outside OpenBao
	// (harbor/admin follows Harbor's Helm-generated Secret via a `Replace`
	// PushSecret). Age tracks the SOURCE's rotation, not OpenBao's, so an
	// overdue reading is a statement about Harbor, not about the vault.
	CredClassTracksSource = "tracks-source"
	// CredClassOnDemand — a real rotation path exists, but an OPERATOR triggers
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
	CredClassOnDemand = "on-demand"
	// CredClassStatic — seeded once by bootstrap (or by an operator) and never
	// touched again by any automation: the Harbor robots, the two GitHub-PAT
	// copies, the Slack webhook. This is the `static` row docs/secrets.md's
	// rotation legend has always had and the metric taxonomy did not — so these
	// paths published NO series at all and were invisible on the single pane
	// rather than visibly old. Re-seeding by hand is the only thing that lowers
	// the age, so like the two classes above they get the yearly info nudge,
	// never the 90d SLA.
	CredClassStatic = "static"
)

// credPath is one tracked KV path. Named rather than anonymous because the
// sampler copies and extends this slice, and every one of those literals had to
// repeat the shape verbatim.
type CredPath struct {
	Path, Cred, Class string
	Optional          bool
}

var CredPaths = []CredPath{
	// secret/loki/object-store AND secret/harbor/registry-s3 ARE NOT HERE, and
	// their absence is deliberate. Both were per-app Linode Object Storage keys,
	// projected into the cluster by ExternalSecrets that 52465691 deleted when
	// object storage went apl-core-native: apl-core builds its own credential
	// Secret from `obj-secrets` and never reads LLZ's. The ExternalSecrets went;
	// this table, the rotation table and the OpenBao policies did not — so for
	// months the platform minted and rotated two read_write keys (the Loki one
	// spanning chunks/ruler/admin, which hold the OpenBao audit stream) into paths
	// nothing read, and age-tracked them green on the single pane.
	//
	// `secret/obj/platform` below is the one that replaced them: one broad key,
	// one consumer, one series.
	{"secret/obj/platform", "obj-platform", CredClassAutomated, false},
	// The narrow in-cluster PAT. Re-minted monthly per region by
	// secret-rotation.yml → `rotate-incluster-pat`, so it is genuinely
	// `automated` and belongs on the 90d SLA: a stalled rotation workflow is
	// exactly the failure this alert can ask a human to fix. It was the one
	// credential with real rotation and no watchdog over it.
	{"secret/linode/api-token", "linode-incluster-pat", CredClassAutomated, false},
	// The BROAD account read_write PAT — the highest-privilege Linode credential
	// the platform holds. The broadPatRotator CronJob re-mints it weekly against
	// ROTATE_AFTER_DAYS, so it is `automated` and belongs on the 90d SLA for the
	// same reason the narrow PAT does. Its EXPIRY already rode in free via the
	// token-inventory's Linode enumeration; what was missing is rotation age —
	// so a wedged rotator stayed invisible until the token actually lapsed.
	{"secret/linode/broad-pat", "linode-broad-pat", CredClassAutomated, false},
	{"secret/grafana/admin", "grafana-admin", CredClassGenerateOnce, false},
	{"secret/otel/ingress", "otel-ingress", CredClassGenerateOnce, false},
	{"secret/harbor/admin", "harbor-admin", CredClassTracksSource, false},
	// Bootstrap seeds, re-seeded only by hand. `secret/alerts/webhooks` is
	// operator-seeded and only when spec.alerting.receivers includes slack; an
	// instance without it 404s and is skipped, same as any unseeded path.
	{"secret/harbor/robot", "harbor-robot", CredClassStatic, false},
	{"secret/harbor/pull-robot", "harbor-pull-robot", CredClassStatic, false},
	{"secret/cert-automation/github-token", "cert-automation-github-token", CredClassStatic, false},
	{"secret/infra/github-dispatch-token", "infra-github-dispatch-token", CredClassStatic, false},
	// The third copy of a GitHub PAT held in OpenBao, and the same drift signal as
	// the two above: token-inventory measures the GitHub-side expiry of
	// APL_VALUES_REPO_TOKEN, but nothing re-seeds THIS copy when an operator
	// rotates that PAT, and a stale copy is what actually breaks apl-core's
	// otomi.git and the argocd repo Secrets.
	{"secret/infra/apl-values-repo-token", "infra-apl-values-repo-token", CredClassStatic, false},
	{"secret/alerts/webhooks", "alerts-webhooks", CredClassStatic, false},
	// OPT-IN least-privilege firewall token (docs/consume-lke-landing-zone-internal.md).
	// Most instances never seed it — the sampler 404s and publishes nothing, same
	// as any unseeded path. Where it IS seeded it is operator-managed on a
	// documented ≤90d policy, so `on-demand` (actionable, 90d SLA) rather than
	// `static` (yearly nudge) is what matches the posture the docs promise.
	{"secret/linode/cloud-firewall", "linode-cloud-firewall", CredClassOnDemand, true},
}

const DBAdminRoot = "secret/infra/db-admin"
