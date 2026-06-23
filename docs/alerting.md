# Alerting

This page catalogues every item in the platform that requires an alert, where the
alert is defined, and its current coverage status. It is the inventory; the
operational response for each is in the relevant runbook.

## Where alerts come from

There are four independent alerting mechanisms in this template. They are
deliberately layered — the GitHub Actions checks are belt-and-suspenders that
fire even if the in-cluster observability stack (kube-prometheus-stack, Grafana,
Loki) is itself broken.

| Mechanism | Defined in | Notification path |
|-----------|------------|-------------------|
| Prometheus rules (custom) | `PrometheusRule` CRs under [apl-values/components/observability/prometheus-rules/](../instance-template/apl-values/components/observability/prometheus-rules/) — deployed source of truth, synced by apl-core's Argo CD and picked up by kube-prometheus-stack's `ruleSelector` | Prometheus UI / Grafana (see caveat below) |
| Prometheus rules (defaults) | `kube-prometheus-stack.defaultRules.create: true` — node, kubelet, kube-state, and Prometheus self-monitoring | Prometheus UI / Grafana |
| Scheduled CI checks | [.github/workflows/scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) | GitHub Actions `::warning::`/`::error::` annotations + job failure |
| Rotation failure notice | the AppRole-rotation CronWorkflow `onExit` handler (under the OpenBao Argo CD application templates) | Opens a GitHub issue |

> **Caveat — no paging yet.** `kube-prometheus-stack.alertmanager.enabled` is
> `false` in [apl-values/_shared/values.yaml](../instance-template/apl-values/_shared/values.yaml).
> Prometheus evaluates every rule and shows firing alerts in its own UI and via
> Grafana, but there is **no Alertmanager routing / paging** for them today. The
> only alerts that actively reach a human right now are the GitHub Actions
> annotations, the AppRole-rotation GitHub issue, and any Grafana-configured
> alerts (e.g. a TLS cert-expiry alert). Wire up Alertmanager (or Grafana
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

### TLS certificates

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| cert-manager Certificates | `Ready=False` | `certmanager-health` job in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) (daily) | ✅ covered |

### Credential rotation

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| OpenBao AppRole `secret_id` rotation failure | CronWorkflow run fails | `onExit` handler opens a GitHub issue → run [docs/runbooks/approle-rotation.md](runbooks/approle-rotation.md) | ✅ covered |
| AppRole rotation overdue | Last success > threshold (secret_id expires ~92 days) | `scheduled-checks.yml → approle-rotation-health` emits `::warning::` | ✅ covered |
| `lke-admin-token` rotation overdue | Newest Secret age ≥35d (warn) / ≥90d (job red) | `scheduled-checks.yml → lke-admin-rotation-health` → [docs/runbooks/lke-admin-rotation.md](runbooks/lke-admin-rotation.md) | ✅ covered |
| Linode PAT expiry policy breach | Any PAT with no expiry / >90d lifetime / expired (warn ≤14d before expiry) | `scheduled-checks.yml → linode-pat-expiry-health` runs the Linode credential audit tool (exit 1 → job red) → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered |
| github.com service PAT expiry breach | Named service PAT with no expiry / >90d / 401 (warn ≤14d) | `scheduled-checks.yml → gh-pat-expiry-health` — per-token `GitHub-Authentication-Token-Expiration` header self-check (job red) → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered (named service PATs) |
| Ad-hoc individual classic PATs | — | **Manual** — GitHub has no classic-PAT list API; enterprise audit-log / admin review only | ⚠️ manual only |
| Loki object-storage bucket key overdue (≤120d) | `secret/loki/object-store` version age ≥105d (warn) / ≥120d (job red) | `scheduled-checks.yml → loki-objkey-rotation-health`; declarative `time_rotating` replacement in the `object-storage` Terraform module | ✅ covered |
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
