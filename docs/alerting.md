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

> **Alertmanager runs; notification needs a one-time opt-in.** Alertmanager is
> enabled (`apps.alertmanager.enabled: true` in
> [apl-values/_shared/values.yaml](../instance-template/apl-values/_shared/values.yaml))
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
   ([kyverno-policies/](../instance-template/apl-values/_shared/manifest/kyverno-policies/))
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
[apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/)).

| Condition | Alert | Severity | Status |
|-----------|-------|----------|--------|
| Scrape target down for 5m | `OpenBaoMetricsTargetDown` | critical | ✅ covered |
| Pod sealed for 2m | `OpenBaoSealed` | critical | ✅ covered |
| No active leader for 2m | `OpenBaoNoActiveLeader` | critical | ✅ covered |
| Raft quorum degraded (< 3 unsealed pods) | `OpenBaoRaftQuorumDegraded` | warning | ✅ covered |
| Uninitialized | — | — | ⚠️ gap |
| Login error rate high | — | — | ⚠️ gap (needs metric verification: `vault_audit_*` / `vault_core_handle_login_request`) |
| Token lease exhaustion | — | — | ⚠️ gap (needs metric verification: `vault_token_*` / `vault_expire_num_leases`) |
| Audit log device down | — | — | ⚠️ gap (needs metric verification: `vault_audit_log_request_failure`) |

### Observability / support plane

Covered by `support-plane-alerts` (under
[apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/)).
Two layers now: the original **scrape-health** alerts (`...MetricsTargetDown`,
`up == 0`) plus **workload-availability** alerts (`SupportPlaneDeploymentUnavailable`,
`LokiStatefulSetUnavailable`) that fire on zero available/ready replicas via
kube-state-metrics — a pod that is Running-but-NotReady scrapes fine yet serves
nothing. Deeper per-service *error-rate/saturation* coverage remains a gap: those
need service-internal exporter metric names (`otelcol_*`, `loki_*`, `harbor_*`)
that promtool can't verify exist, so they want a one-time spot-check against a live
`/metrics` before shipping.

| Service | Scrape-health | Availability | Error-rate / saturation (gap) |
|---------|---------------|--------------|-------------------------------|
| OTel Collector | `OTelCollectorMetricsTargetDown` ✅ | `SupportPlaneDeploymentUnavailable` ✅ | ⚠️ dropped/refused spans, exporter send failures, queue length, `memory_limiter` near limit |
| Loki | `LokiMetricsTargetDown` ✅ | `LokiStatefulSetUnavailable` ✅ | ⚠️ ingestion/query 5xx, object-store write errors, WAL replay, compactor stalled |
| Grafana | `GrafanaMetricsTargetDown` ✅ | `SupportPlaneDeploymentUnavailable` ✅ | — (availability is the main concern) |
| Harbor | `HarborMetricsTargetDown` ✅ | `SupportPlaneDeploymentUnavailable` ✅ (core + registry) | ⚠️ DB connection failures, Trivy scan queue depth, registry disk > 80% |
| Prometheus | (self — via `defaultRules`) | (via `defaultRules`) | ⚠️ confirm TSDB compaction failures + scrape-duration are covered by defaults |

The desired end-state coverage bar is one availability + one error-rate + one
resource-saturation alert per service — availability is now covered; error-rate/
saturation is the remaining gap.

### Visualizing the in-cluster signal

The reconciler's day-2 gauges (convergence, ESO/cert readiness, OpenBao seal,
credential age, per-reconciler status) are surfaced in the **LLZ Day-2** Grafana
dashboard ([llz-day2-dashboard.yaml](../instance-template/apl-values/components/observability/llz-day2-dashboard.yaml),
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
| github.com service PAT expiry breach | Named service PAT with no expiry / >90d / 401 (warn ≤14d) | `scheduled-checks.yml → gh-pat-expiry-health` — per-token `GitHub-Authentication-Token-Expiration` header self-check (job red) → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered (named service PATs) |
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
   [apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/)
   (deployed source of truth) and reference it from that directory's
   `kustomization.yaml`.
2. If you add a new rule group, also add it to the `EXPECTED_RULES` list in the
   rule-drift check in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml)
   so its absence is detected.
3. Argo CD syncs the rule into Prometheus on the next reconcile.
