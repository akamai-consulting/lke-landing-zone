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
| Prometheus rules (custom) | `PrometheusRule` CRs under [platform-apl/components/observability/prometheus-rules/](../instance-template/platform-apl/components/observability/prometheus-rules/) — deployed source of truth, synced by apl-core's Argo CD and picked up by kube-prometheus-stack's `ruleSelector` | Prometheus UI / Grafana (see caveat below) |
| Prometheus rules (defaults) | `kube-prometheus-stack.defaultRules.create: true` — node, kubelet, kube-state, and Prometheus self-monitoring | Prometheus UI / Grafana |
| Scheduled CI checks | [.github/workflows/scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) | GitHub Actions `::warning::`/`::error::` annotations + job failure |

> **Alertmanager runs; notification needs a one-time opt-in.** Alertmanager is
> enabled (`apps.alertmanager.enabled: true` in
> [apl-values/values.yaml](../instance-template/apl-values/values.yaml))
> and every firing rule reaches it — but the default receiver set is `[none]`
> (a null route), so until an instance wires a receiver the only alerts that
> actively reach a human are the GitHub Actions annotations. See **Wiring a
> notification receiver** below; do it before production.

## Wiring a notification receiver (Slack)

The receiver config is spec-driven and the webhook secret lives in OpenBao —
no GitHub secret, no values churn:

1. **Spec** — in `landingzone.yaml`:

   ```yaml
   spec:
     alerting:
       receivers: [slack]
       slack:                      # optional; defaults mon-apl / mon-apl-crit
         channel: platform-alerts
         channelCrit: platform-alerts-crit
   ```

   then `llz render`: the receivers + channels land in every env's committed
   values.yaml `alerts:` block, and apl-core renders the full Alertmanager
   route/receiver config from it (critical-severity alerts go to
   `channelCrit`, the rest to `channel`).

2. **Webhook secret** — seed the Slack webhook URL into each env's OpenBao
   (dual-write on HA pairs):

   ```bash
   llz openbao set alerts/webhooks slack_url=https://hooks.slack.com/services/…
   ```

   apl-core mounts the URL from the `alertmanager-credentials` Secret; the
   `kyverno-alertmanager-slack-webhook` policy
   ([kyverno-policies/](../instance-template/platform-apl/manifest/kyverno-policies/))
   repoints that Secret's ExternalSecret at the `openbao` store, so ESO picks
   the seed up within its 5m refresh. Rotation is the same `llz openbao set`
   again. An unseeded path leaves the ExternalSecret NotReady — a loud, named
   failure, not silently-dead notifications.

3. **Verify** — fire a test alert (e.g. `amtool alert add …` against the
   Alertmanager API, or temporarily scale a watched Deployment to 0) and
   confirm the Slack message.

`msteams` is deliberately not surfaced: apl-core renders its webhook URLs
inline from values (x-secret), which would put secret material into the
committed values flow the OpenBao path exists to avoid.

> **Scheduled CI checks are belt-and-suspenders, not the primary signal.** The
> in-cluster llz-reconciler samples OpenBao seal, ESO-store + cert-manager
> readiness, convergence, and credential age continuously and raises Prometheus
> alerts — so the CIDR-fragile hosted-runner probes that duplicated that coverage
> were **demoted from daily to weekly**: `health-openbao` (ESO) + `health-certmanager`
> (via `LLZESOStoreNotReady` / `LLZCertificatesNotReady`), and
> `health-loki-objkey-rotation` (via `LLZCredentialRotationOverdue`, which alerts on
> `llz_credential_age_days > 90` for both the Loki and Harbor object-storage keys).
> They still fire even when the observability stack itself is broken, and cover a
> cluster whose operator has not wired a receiver. The remaining daily jobs
> (`lke-admin-rotation`, Linode/GitHub PAT expiry) check external credentials the
> reconciler cannot see in-cluster and stay daily.

## Items that require alerts

The product workloads you deploy on top of this landing zone will add their own
custom Prometheus rule groups (one availability + one error-rate +
one resource-saturation alert per service is a reasonable coverage bar). The
inventory below covers the alerts the **platform itself** ships.

### Secrets plane — OpenBao

Covered by `openbao-alerts` (under
[platform-apl/components/observability/prometheus-rules/](../instance-template/platform-apl/components/observability/prometheus-rules/)).

| Condition | Alert | Severity | Status |
|-----------|-------|----------|--------|
| Scrape target down for 5m | `OpenBaoMetricsTargetDown` | critical | ✅ covered |
| Pod sealed for 2m | `OpenBaoSealed` | critical | ✅ covered |
| No active leader for 2m | `OpenBaoNoActiveLeader` | critical | ✅ covered |
| Raft quorum degraded (< 3 unsealed pods) | `OpenBaoRaftQuorumDegraded` | warning | ✅ covered |
| Token lease exhaustion | `OpenBaoLeaseExhaustion` | warning | ✅ covered (`vault_expire_num_leases > 100k`, tune per steady-state) |
| Audit log device failing | `OpenBaoAuditLogFailure` | critical | ✅ covered (`vault_audit_log_request_failure` — a full audit failure self-seals OpenBao) |
| Uninitialized | — | — | ⚠️ gap (no reliable native `vault_` gauge; an uninitialized cluster has no leader → `OpenBaoNoActiveLeader` covers it in practice) |
| Login error rate high | — | — | ⚠️ gap (no clean core error-rate metric; `vault_core_handle_login_request` is latency/count only) |

> **All `vault_*` alerts depend on the :8200 metrics scrape.** Before the
> `llz-openbao-platform` 0.1.18 NetworkPolicy fix in this branch, apl-core's
> Prometheus (in the `monitoring` namespace) was L4-blocked from OpenBao :8200, so
> **every** `vault_*` series was absent and all six OpenBao alerts read `DEAD?` in
> `llz ci alert-eval` on a converged cluster — silently never-firing. The fix adds
> a pod-scoped `allowedClientPods` grant for the Prometheus pod. Verify post-fix:
> `llz ci prom-metrics --match '^vault_'` should return a non-empty set.

### Observability / support plane

Covered by `support-plane-alerts` (under
[platform-apl/components/observability/prometheus-rules/](../instance-template/platform-apl/components/observability/prometheus-rules/)).
Two layers now: the original **scrape-health** alerts (`...MetricsTargetDown`,
`up == 0`) plus **workload-availability** alerts (`SupportPlaneDeploymentUnavailable`,
`LokiStatefulSetUnavailable`) that fire on zero available/ready replicas via
kube-state-metrics — a pod that is Running-but-NotReady scrapes fine yet serves
nothing. The third + fourth layers — **error-rate** and **saturation** — need
service-internal exporter metric names (`otelcol_*`, `loki_*`, `harbor_*`) that
promtool can't verify exist, so each was checked against a live `/metrics` with
`llz ci prom-metrics` before shipping (see the per-service status below).

| Service | Scrape-health | Availability | Error-rate / saturation |
|---------|---------------|--------------|-------------------------|
| OTel Collector | `OTelCollectorMetricsTargetDown` ✅ | `SupportPlaneDeploymentUnavailable` ✅ | 🟡 `OTelCollectorRefusingData` (memory_limiter/backpressure) + `OTelCollectorExportFailures` — **provisional**: `otelcol_*` only scrapes after the 0.1.8 NP fix below, and the pipeline is still a placeholder (debug exporter), so these read `DEAD?`/quiet until a real exporter + the fix land |
| Loki | `LokiMetricsTargetDown` ✅ | `LokiStatefulSetUnavailable` ✅ | ✅ `LokiRequestErrors` (5xx ratio) + `LokiObjectStoreErrors` (S3 Put/Get 5xx, List excluded) + `LokiIngestionDiscarding` — **verified live** against 271 real `loki_*` series (armed, not false-firing) |
| Grafana | `GrafanaMetricsTargetDown` ✅ | `SupportPlaneDeploymentUnavailable` ✅ | — (availability is the main concern) |
| Harbor | `HarborMetricsTargetDown` ✅ (retargeted) | `SupportPlaneDeploymentUnavailable` ✅ (core + registry) | 🟡 `HarborComponentDown` (`harbor_up`) + `HarborCoreHighErrorRate` (core 5xx ratio) + `HarborJobQueueBacklog` (`harbor_task_queue_size`) — **provisional** (issue #183): this branch enables the exporter (`harbor._rawValues.metrics`) + its ServiceMonitor + the `monitoring`→`:8001` NP, but `harbor_*` only appears once that converges, so `alert-eval` reads these `DEAD?` until then. `HarborMetricsTargetDown` was retargeted off the CNPG DB (`harbor-otomi-db`) onto the real `harbor-*` targets. Registry-disk saturation is N/A (registry → S3, not a PVC). |
| Prometheus | (self — via `defaultRules`) | (via `defaultRules`) | ⚠️ confirm TSDB compaction failures + scrape-duration are covered by defaults |

The desired end-state coverage bar is one availability + one error-rate + one
resource-saturation alert per service. Availability is covered fleet-wide; Loki is
fully covered (verified); OpenBao gained lease/audit coverage; OTel and Harbor are
provisional (Harbor's exporter is enabled by this branch but `harbor_*` and the
alert thresholds still want a live spot-check once the first e2e converges —
`llz ci prom-metrics --match '^harbor_'` + `alert-eval`, then add the Harbor
ServiceMonitor to `defaultScrapeMonitors` in `ci_assert_scrape.go` to gate it).

> **The OTel `:8888` scrape depends on a NetworkPolicy, like OpenBao's.** The
> `otel-collector-monitoring` ServiceMonitor selects the target, but until the
> `llz-cluster-foundation` 0.1.8 fix in this branch, `observability-allow-ingress`
> had no rule for apl-core's Prometheus (`monitoring` namespace), so every scrape
> of the collector's `:8888` telemetry port timed out and `otelcol_*` was absent
> cluster-wide (confirmed live). The fix adds a `monitoring`→`:8888` ingress rule
> scoped to the metrics port. Verify post-fix: `llz ci prom-metrics --match
> '^otelcol_'` should return a non-empty set.

**E2E wiring gate.** The scrape-health alerts above are only as good as the
scrape wiring they sit on: a ServiceMonitor/PrometheusRule that loses its
`prometheus: system` label (or a renamed Service port / wrong namespaceSelector)
leaves the CR present but silently un-scraped/un-loaded, and `converge` /
`health` / `assert-loki` all stay green. The release-e2e converge now gates on
`llz ci assert-scrape-targets` (every landing-zone ServiceMonitor has a live `up`
target and every PrometheusRule group is loaded), so that class of regression
fails the e2e instead of shipping a metrics surface that quietly stopped flowing.
The companion `llz ci alert-eval` runs report-only (its FIRING/ARMED/`DEAD?`/`BROKEN`
report is surfaced in the job summary) and is intended to harden to `--strict`
once the last opt-in-reconciler `DEAD?` alerts are resolved.

Two further gates run in the same converge:

- **`llz ci assert-reconciler`** — the reconciler's *functional* health, which
  pod phase can't see: `llz_reconcile_up == 1` (the reconcile loop is up AND its
  samples succeed — a pod Running yet failing on a permission dropped by the
  least-privilege RBAC, or lost OpenBao/Linode access, reports 0) and
  `llz_reconcile_leader == 1` (a replica holds the driving Lease). `alert-eval
  --strict` can't cover this — the matching `LLZReconcilerReportingDown` /
  `LLZReconcilerNoLeader` alerts would be *firing*, and `--strict` ignores FIRING.
- **`llz ci wave-health-audit --fail-on-unvetted`** — the runtime counterpart to
  the static wave-health-guard + VAP: it enumerates every live negative-sync-wave
  resource and fails on any kind the VAP would DENY (a coverage gap or a latent
  false-positive), instead of waiting for the weekly scheduled audit.

### Visualizing the in-cluster signal

The reconciler's day-2 gauges (convergence, ESO/cert readiness, OpenBao seal,
credential age, per-reconciler status) are surfaced in the **LLZ Day-2** Grafana
dashboard ([llz-day2-dashboard.yaml](../instance-template/platform-apl/components/observability/llz-day2-dashboard.yaml),
a ConfigMap the Grafana dashboard sidecar auto-imports). This is the at-a-glance
view for a receiver-less operator — alerts aggregate in Alertmanager but notify
nobody until a receiver is wired, so the dashboard is their window.

### TLS certificates

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| cert-manager Certificates | `Ready=False` | in-cluster `LLZCertificatesNotReady` alert (continuous) + `certmanager-health` job in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) (weekly, belt-and-suspenders) | ✅ covered |

### Credential rotation

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| `lke-admin-token` rotation overdue | Newest Secret age ≥35d (warn) / ≥90d (job red) | `scheduled-checks.yml → lke-admin-rotation-health` → [docs/runbooks/lke-admin-rotation.md](runbooks/lke-admin-rotation.md) | ✅ covered |
| Linode PAT expiry policy breach | Any PAT with no expiry / >90d lifetime / expired (warn ≤14d before expiry) | `scheduled-checks.yml → linode-pat-expiry-health` runs the Linode credential audit tool (exit 1 → job red) → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered |
| github.com service PAT expiry breach | Named service PAT with no expiry / >90d / 401 (warn ≤14d) | `scheduled-checks.yml → credential single pane` — `llz ci token-inventory` measures each token's `GitHub-Authentication-Token-Expiration` header into the `llz-token-inventory` ConfigMap; the reconciler exports `llz_token_expiry_*` and `LLZToken*` alerts fire → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered (named service PATs) |
| Ad-hoc individual classic PATs | — | **Manual** — GitHub has no classic-PAT list API; enterprise audit-log / admin review only | ⚠️ manual only |
| Loki object-storage bucket key overdue (≤120d) | `secret/loki/object-store` version age ≥105d (warn) / ≥120d (job red) | in-cluster `LLZCredentialRotationOverdue` alert (>90d, continuous) + `scheduled-checks.yml → loki-objkey-rotation-health` (weekly, belt-and-suspenders); declarative `time_rotating` replacement in the `object-storage` Terraform module | ✅ covered |
| TF-state object-storage bucket key overdue (≤120d) | — | **Manual, calendar-tracked** — bootstrapping paradox (the key guards the state any automation would need). No automated alert possible. | ⚠️ manual only |
| Prometheus rule drift | Expected rule groups missing from cluster | `scheduled-checks.yml` — surfaces silently-broken alerting before an incident | ✅ covered |

### Cluster / platform

Node pressure, kubelet health, kube-state anomalies and Prometheus
self-monitoring are covered by the kube-prometheus-stack default rules
(`defaultRules.create: true`). No custom rules are maintained for these in this
template.

## Adding or changing an alert

1. Edit (or add) the matching `PrometheusRule` file under
   [platform-apl/components/observability/prometheus-rules/](../instance-template/platform-apl/components/observability/prometheus-rules/)
   (deployed source of truth) and reference it from that directory's
   `kustomization.yaml`.
2. If you add a new rule group, also add it to the `EXPECTED_RULES` list in the
   rule-drift check in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml)
   AND to `defaultScrapeRuleGroups` in
   [tools/cmd/llz/ci_assert_scrape.go](../tools/cmd/llz/ci_assert_scrape.go) — the
   e2e `assert-scrape-targets` gate fails if an expected group isn't loaded into
   Prometheus. Likewise, a new landing-zone ServiceMonitor goes in that file's
   `defaultScrapeMonitors` so the e2e asserts it actually produces an `up` target.
3. Argo CD syncs the rule into Prometheus on the next reconcile.
