# Alerting

This page catalogues every item in the platform that requires an alert, where the
alert is defined, and its current coverage status. It is the inventory; the
operational response for each is in the relevant runbook.

## Where alerts come from

There are three independent alerting mechanisms in this template. They are
deliberately layered — the GitHub Actions checks are belt-and-suspenders that
fire even if the in-cluster observability stack (kube-prometheus-stack, Grafana,
Loki) is itself broken.

| Mechanism | Defined in | Notification path |
|-----------|------------|-------------------|
| Prometheus rules (custom) | `PrometheusRule` CRs under [apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/) — deployed source of truth, synced by apl-core's Argo CD and picked up by kube-prometheus-stack's `ruleSelector` | Prometheus UI / Grafana (see caveat below) |
| Prometheus rules (defaults) | `kube-prometheus-stack.defaultRules.create: true` — node, kubelet, kube-state, and Prometheus self-monitoring | Prometheus UI / Grafana |
| Scheduled CI checks | [.github/workflows/scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) | GitHub Actions `::warning::`/`::error::` annotations + job failure |

> **Caveat — no paging yet.** `kube-prometheus-stack.alertmanager.enabled` is
> `false` in [apl-values/_shared/values.yaml](../instance-template/apl-values/_shared/values.yaml).
> Prometheus evaluates every rule and shows firing alerts in its own UI and via
> Grafana, but there is **no Alertmanager routing / paging** for them today. The
> only alerts that actively reach a human right now are the GitHub Actions
> annotations and any Grafana-configured alerts (e.g. a TLS cert-expiry
> alert). Wire up Alertmanager (or Grafana
> notification policies) before production.

## Items that require alerts

The product workloads you deploy on top of this landing zone will add their own
custom Prometheus rule groups (one availability + one error-rate +
one resource-saturation alert per service is a reasonable coverage bar). The
inventory below covers the alerts the **platform itself** ships.

### Secrets plane — OpenBao

Covered by `openbao-alerts` (under
[apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/)).

| Condition | Alert | Severity | Status |
|-----------|-------|----------|--------|
| Scrape target down for 5m | `OpenBaoMetricsTargetDown` | critical | ✅ covered |
| Pod sealed for 2m | `OpenBaoSealed` | critical | ✅ covered |
| No active leader for 2m | `OpenBaoNoActiveLeader` | critical | ✅ covered |
| Uninitialized | — | — | ⚠️ gap |
| Raft peer count < 3 | — | — | ⚠️ gap |
| Login error rate high | — | — | ⚠️ gap |
| Token lease exhaustion | — | — | ⚠️ gap |
| Audit log device down | — | — | ⚠️ gap |

### Observability / support plane

Covered by `support-plane-alerts` (under
[apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/)).
Today this is **scrape-health only** — each service has a single
`...MetricsTargetDown` alert (`up == 0` for 5m, warning). Deeper per-service
coverage is a known gap.

| Service | Scrape-health alert | Deeper coverage (gap) |
|---------|---------------------|-----------------------|
| OTel Collector | `OTelCollectorMetricsTargetDown` ✅ | ⚠️ dropped/refused spans & metrics, exporter send failures, queue length, `memory_limiter` near limit, no data > 15m |
| Loki | `LokiMetricsTargetDown` ✅ | ⚠️ ingestion-rate errors, query failures, object-store write errors, WAL replay errors, compactor not running |
| Grafana | `GrafanaMetricsTargetDown` ✅ | ⚠️ pod availability (relies on `defaultRules` — confirm or add explicit alert) |
| Harbor | `HarborMetricsTargetDown` ✅ | ⚠️ core/registry pod unavailable, DB connection failures, Trivy scan queue depth, registry disk > 80% |
| Prometheus | (self — via `defaultRules`) | ⚠️ confirm TSDB compaction failures and scrape-duration > 30s are covered by defaults |

The desired end-state coverage bar is one availability + one error-rate + one
resource-saturation alert per service.

### Secret-sync plane — External Secrets Operator

Covered by `eso-alerts` (under
[apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/)),
fed by the `external-secrets` **PodMonitor** in the observability component
(apl-core does not ship ESO or scrape it, so without that PodMonitor these series
are absent). This is the coverage that would have caught the
`harbor-docker-config` wedge (an ExternalSecret that silently never synced).

| Condition | Alert | Severity | Status |
|-----------|-------|----------|--------|
| ESO controller not scraped for 10m | `ESOMetricsTargetDown` | warning | ✅ covered |
| ClusterSecretStore `Ready=False` for 10m (cascades to all ExternalSecrets) | `ClusterSecretStoreNotReady` | critical | ✅ covered |
| An ExternalSecret `Ready=False` for 15m | `ExternalSecretNotReady` | warning | ✅ covered |

### TLS certificates

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| cert-manager Certificates | `Ready=False` | `certmanager-health` job in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) (daily) | ✅ covered |
| cert-manager Certificate `Ready=False` for 1h | `CertManagerCertNotReady` (warning) | `cert-manager-alerts` PrometheusRule, fed by the `cert-manager` ServiceMonitor | ✅ covered |
| Certificate expires in < 7d (renewal stuck) | `CertManagerCertExpiringSoon` (warning) | same | ✅ covered |
| Certificate expires in < 48h | `CertManagerCertExpiringCritical` (critical) | same | ✅ covered |

### Credential rotation

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| `lke-admin-token` rotation overdue | Newest Secret age ≥35d (warn) / ≥90d (job red) | `scheduled-checks.yml → lke-admin-rotation-health` → [docs/runbooks/lke-admin-rotation.md](runbooks/lke-admin-rotation.md) | ✅ covered |
| Linode PAT expiry policy breach | Any PAT with no expiry / >90d lifetime / expired (warn ≤14d before expiry) | `scheduled-checks.yml → linode-pat-expiry-health` runs the Linode credential audit tool (exit 1 → job red) → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered |
| github.com service PAT expiry breach | Named service PAT with no expiry / >90d / 401 (warn ≤14d) | `scheduled-checks.yml → gh-pat-expiry-health` — per-token `GitHub-Authentication-Token-Expiration` header self-check (job red) → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered (named service PATs) |
| Ad-hoc individual classic PATs | — | **Manual** — GitHub has no classic-PAT list API; enterprise audit-log / admin review only | ⚠️ manual only |
| Loki object-storage bucket key overdue (≤120d) | `secret/loki/object-store` version age ≥105d (warn) / ≥120d (job red) | `scheduled-checks.yml → loki-objkey-rotation-health`; declarative `time_rotating` replacement in the `object-storage` Terraform module | ✅ covered |
| TF-state object-storage bucket key overdue (≤120d) | — | **Manual, calendar-tracked** — bootstrapping paradox (the key guards the state any automation would need). No automated alert possible. | ⚠️ manual only |
| Prometheus rule drift | Expected rule groups missing from cluster | `scheduled-checks.yml` — surfaces silently-broken alerting before an incident | ✅ covered |
| Credential-rotation CronJob stopped succeeding | `linode-cred-rotator` no success in 36h (critical), `linode-volume-labeler` no success in 2h (warning), `argo-resync-nudger` no success in 1h (warning) | `job-alerts` PrometheusRule, off kube-state-metrics' `kube_cronjob_status_last_successful_time`. Complements the default `KubeJobFailed` (which only fires on a *failing* Job, not a CronJob that has silently stopped running) | ✅ covered |

### Token & credential inventory (single pane of glass)

A unified inventory of every platform credential — including the CI-managed /
external tokens Prometheus cannot scrape directly (GitHub service PATs, Linode
account PATs + OBJ keys) — funnelled into one Grafana pane and alerted on. The
daily `token-inventory-push` job in
[llz-scheduled-checks.yml](../instance-template/.github/workflows/llz-scheduled-checks.yml)
runs `llz ci token-inventory` (reusing the same Linode + GitHub expiry ladders as
`cred-audit` / `gh-pat-expiry`) and PUTs the `llz_token_*` metrics to the
in-cluster **Pushgateway** (`pushgateway/` in the observability component, scraped
with `honorLabels: true`), per-region group. The
**"LLZ — Token & Credential Inventory"** Grafana dashboard
(`grafana-dashboards/token-inventory-dashboard.yaml`, auto-loaded via the
`grafana_dashboard` ConfigMap label) renders the inventory table + expiry
countdowns alongside the natively-scraped cert-manager certificate expiries.

Covered by `token-inventory-alerts`:

| Condition | Alert | Severity | Status |
|-----------|-------|----------|--------|
| A credential in policy breach (no-expiry / expired / >90d lifetime / invalid) | `TokenAuditBreach` | critical | ✅ covered |
| A credential within the expiry warn window (≤14d) | `TokenExpiringSoon` | warning | ✅ covered |
| The daily inventory push has not landed in >36h (pane going stale) | `TokenInventoryStale` | warning | ✅ covered |

Static-by-design credentials (OpenBao seal/recovery keys, generated admin
passwords) and out-of-band ones (Harbor robots) appear in the inventory with
`status="static"` for completeness — they are listed, not alerted. Keep the
static list in `tools/cmd/llz/ci_token_inventory.go` in sync with the rotation
inventory above.

### Cluster / platform

Node pressure, kubelet health, kube-state anomalies and Prometheus
self-monitoring are covered by the kube-prometheus-stack default rules
(`defaultRules.create: true`). No custom rules are maintained for these in this
template. Generic Job failures are covered by the default `KubeJobFailed` /
`KubeJobNotCompleted` rules; the platform's own rotation CronJobs additionally
get the "stopped succeeding" rules under `job-alerts` (see above).

## Adding or changing an alert

1. Edit (or add) the matching `PrometheusRule` file under
   [apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/)
   (deployed source of truth) and reference it from that directory's
   `kustomization.yaml`.
2. If you add a new rule group, also add it to the `EXPECTED_RULES` list in the
   rule-drift check in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml)
   so its absence is detected.
3. Argo CD syncs the rule into Prometheus on the next reconcile.
