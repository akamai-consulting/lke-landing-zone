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
| Prometheus rules (custom) | `PrometheusRule` CRs under [platform-apl/components/observability/prometheus-rules/](../platform-apl/components/observability/prometheus-rules/) — deployed source of truth, synced by apl-core's Argo CD and picked up by kube-prometheus-stack's `ruleSelector` | Alertmanager → the `spec.alerting` receivers (see below); Prometheus UI / Grafana |
| Prometheus rules (defaults) | `kube-prometheus-stack.defaultRules.create: true` — node, kubelet, kube-state, and Prometheus self-monitoring | Alertmanager → the `spec.alerting` receivers; Prometheus UI / Grafana |
| Scheduled CI checks | [.github/workflows/scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) | GitHub Actions `::warning::`/`::error::` annotations + job failure |

Both Prometheus mechanisms notify through Alertmanager, which pages whatever
`spec.alerting` names — PagerDuty and/or Slack (below).

> **Alertmanager runs; notification needs a one-time opt-in.** Alertmanager is
> enabled by apl-core (it is part of the managed App Platform's observability set;
> LLZ neither enables nor deploys it — see ADR
> [0005](adr/0005-managed-app-platform.md))
> and every firing rule reaches it — but apl-core's default receiver set is `[none]`
> (a null route), so until an instance sets `spec.alerting` the only alerts that
> actively reach a human are the GitHub Actions annotations. See **Wiring a
> notification receiver** below; do it before production.

## Wiring a notification receiver (PagerDuty, Slack)

`spec.alerting` renders into an **`AlertmanagerConfig`** that apl-core's
Alertmanager picks up. Two receiver types are supported, separately or together:

| Receiver | Pages on | Secret (OpenBao, `secret/` mount) |
|---|---|---|
| `pagerduty` | `severity` ∈ `pagerduty.severities` (default `[critical]`) | `alerts/pagerduty`, property `routing_key` — the Events API v2 **integration key** of a PagerDuty service |
| `slack` | `critical` → `slack.channelCrit` (default `mon-apl-crit`); everything else → `slack.channel` (default `mon-apl`) | `alerts/webhooks`, property `slack_url` — an incoming-webhook URL |

`Watchdog` and `InfoInhibitor` are excluded from every receiver: both are firing
by design, and paging on them pages forever.

### How it reaches Alertmanager, and why this way

apl-core builds Alertmanager's *base* config from a top-level `alerts:` values
block that no LLZ channel can reach ([upstream-asks.md](upstream-asks.md) §4 —
still open). But the `Alertmanager` CR apl-core deploys (kube-prometheus-stack,
prometheus-operator v0.93 on apl-core 6.3) leaves `alertmanagerConfigSelector` and
`alertmanagerConfigNamespaceSelector` at the chart default `{}` — it selects
**every** `AlertmanagerConfig` in every namespace and merges each one into the
base config as a sub-route at the **top** of the root route, with
`continue: true`. So LLZ ships its routing as a CR instead of as values:

1. `llz render` writes `apl-values/<env>/apps/observability/alerting.yaml` — an
   `ExternalSecret` and an `AlertmanagerConfig`, both named `llz-alerting` in
   `monitoring` — and lists it in that app's `kustomization.yaml`. It rides the
   `llz-observability` Argo CD app, so a bad CR degrades only that app. (Render
   **fails** if `spec.alerting` names a receiver and an env does not deploy the
   observability component — a pager that silently renders nowhere is the failure
   this replaces.)
2. The `ExternalSecret` materialises the key from OpenBao through the `openbao`
   ClusterSecretStore, like every other platform credential. No key, no Secret —
   and prometheus-operator **rejects** an `AlertmanagerConfig` whose Secret it
   cannot resolve.
3. The apl-overlay sets `apps.alertmanager._rawValues.alertmanagerSpec
   .alertmanagerConfigMatcherStrategy.type: OnNamespaceExceptForAlertmanagerNamespace`
   (`apl-values/_shared/apl-overlay/appvalues.yaml`). **This is load-bearing.**
   Under the operator's default (`OnNamespace`) every `AlertmanagerConfig` route
   gets a `namespace="<the CR's namespace>"` matcher, so a CR in `monitoring`
   would page only for alerts from `monitoring` — not `OpenBaoSealed`
   (`llz-openbao`), not node alerts (no namespace) — while its receiver showed up
   in Alertmanager's config and nothing reported an error. The exemption applies
   only to CRs in Alertmanager's own namespace; CRs anywhere else stay scoped.

**Rejected alternative — an LLZ-owned Alertmanager.** A second Alertmanager would
need Prometheus to send to it (`alertingEndpoints` on apl-core's Prometheus CR —
another apl-core value LLZ cannot set), and would split silences and inhibitions
across two instances. Not needed while the managed one selects CRs.

### Steps

1. **Spec** — in `landingzone.yaml`, then `llz render` and commit:

   ```yaml
   spec:
     alerting:
       receivers: [pagerduty]          # and/or slack
       pagerduty:                      # optional
         severities: [critical]        # default [critical]
         # url: https://events.eu.pagerduty.com/v2/enqueue   # EU-region accounts
       # slack:                        # optional; defaults mon-apl / mon-apl-crit
       #   channel: platform-alerts
       #   channelCrit: platform-alerts-crit
   ```

2. **Secret** — in PagerDuty, add an *Events API v2* integration to the service
   that should be paged and copy its integration key. Seed it into each env's
   OpenBao (dual-write on HA pairs):

   ```bash
   llz openbao set alerts/pagerduty routing_key=<integration key>
   llz openbao set alerts/webhooks slack_url=https://hooks.slack.com/services/…   # if slack
   ```

   Both paths are age-tracked by the reconciler's credential gauges
   (`class="static"`). ESO re-reads them every minute, so a rotated key reaches
   Alertmanager without a restart.

3. **Verify — gate, then a real page.** `llz ci assert-alert-routing` reads the
   config Alertmanager is *running*, requires each receiver with its key set, and
   posts **silenced** probe alerts — from a non-`monitoring` namespace, with no
   namespace, and one that must *not* page — and reads back which receiver
   Alertmanager chose. It never notifies anyone. `llz-scheduled-checks` runs it
   weekly; the release e2e runs it as the `alert-routing` lane.

   The gate stops at Alertmanager's decision. It cannot see whether PagerDuty
   accepted the event (a wrong-but-non-empty key, or egress from the Alertmanager
   pod blocked), so after seeding **fire one real alert once** and confirm the
   page arrives:

   ```bash
   kubectl -n monitoring port-forward svc/alertmanager-operated 9093 &
   amtool --alertmanager.url=http://127.0.0.1:9093 alert add LLZPagingTest \
     severity=critical namespace=llz-paging-test --annotation=summary="paging test — resolve me"
   ```

   Afterwards, `alertmanager_notifications_failed_total{integration="pagerduty"}`
   is where delivery failures show.

**Turning it off.** Remove `spec.alerting` (or set `receivers: [none]`) and
`llz render`: the kustomization stops listing `alerting.yaml`, so Argo prunes
the CR. The stale file stays on disk until you delete it — render only adds
files — but nothing references it.

`msteams` and `opsgenie` are not surfaced. msteams would need its webhook URLs in
committed values; opsgenie could ride the same `AlertmanagerConfig` mechanism and
is a straightforward follow-up if someone needs it.

> **Scheduled CI checks are belt-and-suspenders, not the primary signal.** The
> in-cluster llz-reconciler samples OpenBao seal, ESO-store + cert-manager
> readiness, convergence, and credential age continuously and raises Prometheus
> alerts — so the CIDR-fragile hosted-runner probes that duplicated that coverage
> were **demoted from daily to weekly**: `health-openbao` (ESO) + `health-certmanager`
> (via `LLZESOStoreNotReady` / `LLZCertificatesNotReady`). A third,
> `health-loki-objkey-rotation`, was **retired outright** (#483) rather than demoted:
> it needed `OPENBAO_ROOT_TOKEN`, which bootstrap revokes, so it had never produced
> a verdict. `LLZCredentialRotationOverdue` alerts on
> `llz_credential_age_days > 90` for both object-storage keys, and the daily
> `llz ci assert-rotation-health` gates the same gauge in CI.
> They still fire even when the observability stack itself is broken, and cover a
> cluster whose operator has not set `spec.alerting`. The remaining daily jobs
> (`lke-admin-rotation`, Linode/GitHub PAT expiry) check external credentials the
> reconciler cannot see in-cluster and stay daily.

## Items that require alerts

The product workloads you deploy on top of this landing zone will add their own
custom Prometheus rule groups (one availability + one error-rate +
one resource-saturation alert per service is a reasonable coverage bar). The
inventory below covers the alerts the **platform itself** ships.

### Secrets plane — OpenBao

Covered by `openbao-alerts` (under
[platform-apl/components/observability/prometheus-rules/](../platform-apl/components/observability/prometheus-rules/)).

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

> **All `vault_*` alerts depend on the :8200 metrics scrape**, which a
> NetworkPolicy has to allow. `llz-openbao-platform` grants it with a pod-scoped
> `allowedClientPods` entry for apl-core's Prometheus pod (since chart 0.1.18).
>
> Without that grant Prometheus (in the `monitoring` namespace) is L4-blocked from
> OpenBao :8200, so **every** `vault_*` series is absent and all six OpenBao alerts
> read `DEAD?` in `llz ci alert-eval` on a converged cluster — silently
> never-firing rather than loudly broken. Verify with
> `llz ci prom-metrics --match '^vault_'`; a non-empty set means the grant is live.

### Observability / support plane

Covered by `support-plane-alerts` (under
[platform-apl/components/observability/prometheus-rules/](../platform-apl/components/observability/prometheus-rules/)).
Two layers now: the original **scrape-health** alerts (`...MetricsTargetDown`,
`up == 0`) plus **workload-availability** alerts (`SupportPlaneDeploymentUnavailable`,
`LokiStatefulSetDegraded`, `LokiStatefulSetUnavailable`) that fire on missing or zero ready replicas via
kube-state-metrics — a pod that is Running-but-NotReady scrapes fine yet serves
nothing. The third + fourth layers — **error-rate** and **saturation** — need
service-internal exporter metric names (`otelcol_*`, `loki_*`, `harbor_*`) that
promtool can't verify exist, so each was checked against a live `/metrics` with
`llz ci prom-metrics` before shipping (see the per-service status below).

| Service | Scrape-health | Availability | Error-rate / saturation |
|---------|---------------|--------------|-------------------------|
| OTel Collector | `OTelCollectorMetricsTargetDown` ✅ | `SupportPlaneDeploymentUnavailable` ✅ | 🟡 `OTelCollectorRefusingData` (memory_limiter/backpressure) + `OTelCollectorExportFailures` — **provisional**: `otelcol_*` only scrapes after the 0.1.8 NP fix below, and the pipeline is still a placeholder (debug exporter), so these read `DEAD?`/quiet until a real exporter + the fix land |
| Loki | `LokiMetricsTargetDown` ✅ | `LokiStatefulSetDegraded` + `LokiStatefulSetUnavailable` ✅ (**both** — see the note below the table; the single `== 0` rule they replace could not fire) | ✅ `LokiRequestErrors` (5xx ratio) + `LokiObjectStoreErrors` (S3 Put/Get 5xx, List excluded) + `LokiIngestionDiscarding` — **verified live** against 271 real `loki_*` series (armed, not false-firing) |
| Grafana | `GrafanaMetricsTargetDown` ✅ | `SupportPlaneDeploymentUnavailable` ✅ | — (availability is the main concern) |
| Harbor | `HarborMetricsTargetDown` ✅ (retargeted) | `SupportPlaneDeploymentUnavailable` ✅ (core + registry) | 🟡 `HarborComponentDown` (`harbor_up`) + `HarborCoreHighErrorRate` (core 5xx ratio) + `HarborJobQueueBacklog` (`harbor_task_queue_size`) — defined, but **not scrape-gated** (see below). The exporter (`harbor._rawValues.metrics`), its ServiceMonitor and the `monitoring`→`:8001` NetworkPolicy all ship; `harbor_*` appears once the cluster converges. `HarborMetricsTargetDown` targets the real `harbor-*` targets, not the CNPG database (`harbor-otomi-db`, which also lives in the `harbor` namespace). Registry-disk saturation is N/A — the registry writes to S3, not a PVC. |
| Prometheus | (self — via `defaultRules`) | (via `defaultRules`) | ⚠️ confirm TSDB compaction failures + scrape-duration are covered by defaults |

The desired end-state coverage bar is one availability + one error-rate + one
resource-saturation alert per service. Availability is covered fleet-wide; Loki is
fully covered (verified live); OpenBao carries lease/audit coverage.

**Open gap — Harbor and OTel are not scrape-gated.** `defaultScrapeMonitors` in
`tools/internal/extensions/assertions/assertobs/scrape.go` lists four ServiceMonitors (cert-manager,
otel-collector-monitoring, llz-reconciler, platform-openbao); **Harbor's is not
among them**, so nothing fails if Harbor's metrics stop arriving and its three
alerts quietly go `DEAD?`. Closing it: spot-check the series on a converged
cluster (`llz ci prom-metrics --match '^harbor_'` + `llz ci alert-eval`), confirm
the thresholds against real values, then add the Harbor ServiceMonitor to
`defaultScrapeMonitors`.

> **The OTel `:8888` scrape depends on a NetworkPolicy, like OpenBao's.** The
> `otel-collector-monitoring` ServiceMonitor selects the target;
> `observability-allow-ingress` carries the `monitoring`→`:8888` rule that lets
> apl-core's Prometheus reach it, scoped to the metrics port (since
> `llz-cluster-foundation` 0.1.8).
>
> Without that rule every scrape of the collector's `:8888` telemetry port times
> out and `otelcol_*` is absent cluster-wide — observed live. Verify with
> `llz ci prom-metrics --match '^otelcol_'`; a non-empty set means the rule is live.

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

Four further gates run in the same converge:

- **`llz ci assert-alert-routing`** — the hop after `assert-alert-delivery`:
  Alertmanager → the receiver `spec.alerting` names. Reads the config Alertmanager
  is running and posts silenced probe alerts to see which receiver it picks (see
  **Wiring a notification receiver** above). The release e2e declares a PagerDuty +
  Slack fixture whose endpoints are all `.invalid`, so the lane runs on every
  release; `llz-scheduled-checks` runs it weekly on every instance.

- **`llz ci assert-openbao-audit`** — the same argument applied to the *log* path,
  which `assert-scrape-targets` does not cover and `assert-loki` (Loki is
  bootstrapped) cannot: it reads OpenBao's audit stream back out of Loki and fails
  if nothing arrived in the lookback window. That pipeline shipped nowhere for its
  entire life — `lokiPushUrl` named `loki-gateway.llz-observability`, a Service
  nothing creates, and the NetworkPolicy egress allow named the same empty
  namespace, so the two agreed with each other and neither agreed with the cluster.
  Nothing static can catch that (any URL is consistent with a matching allow) and
  nothing pod-shaped can either (promtail retries a dead name forever and stays
  Running), so the gate is the round trip. Its static half — gate target, chart
  push URL and netpol namespace all agreeing — is a unit test, so a revert fails at
  PR time instead of an e2e cycle later.
- **`llz ci assert-reconciler`** — the reconciler's *functional* health, which
  pod phase can't see: `llz_reconcile_up == 1` (the reconcile loop is up AND its
  samples succeed — a pod Running yet failing on a permission dropped by the
  least-privilege RBAC, or lost OpenBao/Linode access, reports 0) and
  `llz_reconcile_leader == 1` (a replica holds the driving Lease). `alert-eval
  --strict` can't cover this — the matching `LLZReconcilerReportingDown` /
  `LLZReconcilerNoLeader` alerts would be *firing*, and `--strict` ignores FIRING.
- **`llz ci assert-wave-health-vap`** — the runtime counterpart to the static
  wave-health-guard: it server-dry-runs a Deployment at sync-wave -5 and requires
  the `llz-wave-health-guard` VAP's own denial, proving the policy and its Deny
  binding are live. That binding is what makes the static guard's PR-time verdict
  hold against non-CI write paths (an apl-operator writeback, a direct SSA), so an
  unbound guard is a silent regression worth failing on rather than waiting for
  the weekly scheduled check.

### Visualizing the in-cluster signal

The reconciler's day-2 gauges (convergence, ESO/cert readiness, OpenBao seal,
credential age, per-reconciler status) are surfaced in the **LLZ Day-2** Grafana
dashboard ([llz-day2-dashboard.yaml](../platform-apl/components/observability/llz-day2-dashboard.yaml),
a ConfigMap the Grafana dashboard sidecar auto-imports). This is the at-a-glance
view for a receiver-less operator — alerts aggregate in Alertmanager but notify
nobody until `spec.alerting` names a receiver, so the dashboard is their window.

### TLS certificates

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| cert-manager Certificates | `Ready=False` | in-cluster `LLZCertificatesNotReady` alert (continuous) + `health-certmanager` step of `weekly-cluster-checks` in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml) (weekly, belt-and-suspenders) | ✅ covered |

### Credential rotation

| Item | Trigger | Mechanism | Status |
|------|---------|-----------|--------|
| `lke-admin-token` rotation overdue | Newest Secret age ≥35d (warn) / ≥90d (job red) | `scheduled-checks.yml → lke-admin-rotation-health` → [docs/runbooks/lke-admin-rotation.md](runbooks/lke-admin-rotation.md) | ✅ covered |
| Linode PAT expiry policy breach | Any PAT with no expiry / >90d lifetime / expired (warn ≤14d before expiry) | `scheduled-checks.yml → credential-single-pane` runs the Linode credential audit tool (exit 1 → job red) → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered |
| github.com service PAT expiry breach | Named service PAT with no expiry / >90d / 401 (warn ≤14d) | `scheduled-checks.yml → credential single pane` — `llz ci token-inventory` measures each token's `GitHub-Authentication-Token-Expiration` header into the `llz-token-inventory` ConfigMap; the reconciler exports `llz_token_expiry_*` and `LLZToken*` alerts fire → [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) | ✅ covered (named service PATs) |
| Ad-hoc individual classic PATs | — | **Manual** — GitHub has no classic-PAT list API; enterprise audit-log / admin review only | ⚠️ manual only |
| Loki object-storage bucket key overdue (≤120d policy, enforced at 90d) | `llz_credential_age_days{cred="loki-object-store"}` > 90d, **or no series at all** | in-cluster `LLZCredentialRotationOverdue` alert (>90d, continuous) + `scheduled-checks.yml → credential-single-pane → llz ci assert-rotation-health` (daily, and the only one that can fail on an absent series); rotated by the in-cluster `linodeCredRotator` lane every ~80d. The weekly `loki-objkey-rotation-health` step was retired in #483 — it needed a revoked root token and never fired. | ✅ covered |
| TF-state object-storage bucket key overdue (≤120d) | — | Rotated by `secret-rotation.yml` (`scope: tf-state-key` / `tf-state-key-revoke`) outside Terraform. Linode exposes no OBJ-key creation time, so the SLA itself is calendar-tracked. | ⚠️ manual only |
| Prometheus rule drift | Expected rule groups missing from cluster | `scheduled-checks.yml` — surfaces silently-broken alerting before an incident | ✅ covered |

### Cluster / platform

Node pressure, kubelet health, kube-state anomalies and Prometheus
self-monitoring are covered by the kube-prometheus-stack default rules
(`defaultRules.create: true`). No custom rules are maintained for these in this
template.

## Adding or changing an alert

1. Edit (or add) the matching `PrometheusRule` file under
   [platform-apl/components/observability/prometheus-rules/](../platform-apl/components/observability/prometheus-rules/)
   (deployed source of truth) and reference it from that directory's
   `kustomization.yaml`.
2. If you add a new rule group, also add it to the `EXPECTED_RULES` list in the
   rule-drift check in [scheduled-checks.yml](../instance-template/.github/workflows/scheduled-checks.yml)
   AND to `defaultScrapeRuleGroups` in
   [tools/internal/extensions/assertions/assertobs/scrape.go](../tools/internal/extensions/assertions/assertobs/scrape.go) — the
   e2e `assert-scrape-targets` gate fails if an expected group isn't loaded into
   Prometheus. Likewise, a new landing-zone ServiceMonitor goes in that file's
   `defaultScrapeMonitors` so the e2e asserts it actually produces an `up` target.
3. Argo CD syncs the rule into Prometheus on the next reconcile.

## A rule that parses is not a rule that fires

`LokiStatefulSetUnavailable` shipped as
`kube_statefulset_status_replicas_ready{namespace="monitoring", statefulset="loki"} == 0`
and was incapable of firing, for two independent reasons:

- **The name.** apl-core runs the Loki chart's *distributed* topology, so the
  StatefulSet is `loki-ingester`. The selector matched no series, and an
  expression over an empty vector is never true.
- **The threshold.** `== 0` asks for a total outage. The failure that actually
  happened was 1 of 3 ingesters ready for 16 days — ingestion degraded, node
  drains blocked — which a zero-replica test grades healthy.

`promtool check rules` passed on it every day. So did `llz ci alert-eval`, which
graded it **ARMED**: its `DEAD?` detection asks whether any metric *name* in the
expression exists, and `kube_statefulset_status_replicas_ready` very much does.
The name was right and the label was wrong, and name-level detection cannot tell
those apart.

Three things changed, and it is worth being clear that only the third would have
caught it on its own:

1. The rule was split in two (`LokiStatefulSetDegraded` on `< desired`,
   `LokiStatefulSetUnavailable` on `== 0`) and matches `statefulset=~"loki.*"`.
2. `alert-eval` now probes an ARMED rule's **label selectors** and annotates
   `NOMATCH` when every one of them matches zero series. Reported, never gating —
   a counter that has never incremented also matches nothing, and that is healthy,
   so failing on it would make the job red on good clusters and it would stop
   being read.
3. **The rules are executed against named series in CI.**
   `TestSupportPlaneAlertSemantics` runs promtool's rule unit tests over the
   shipped CRD with `loki-ingester` series at 1-of-3 ready, and requires the alert
   to fire. Restoring either half of the original bug turns it red.

And a fourth gap sat underneath all three: **no scheduled job evaluated these
rules at all.** The weekly single-pane job filters to
`^LLZ(Token|Certificate|Credential)`, so the Loki/Harbor/Grafana/OTel rules were
syntax-checked at PR time and never run again. `llz-scheduled-checks.yml` now
carries a second `alert-eval` step for them, and
`TestSupportPlaneAlertsMatchTheScheduledFilter` fails if a shipped alert is named
outside every filter — a rule no job selects is indistinguishable from a healthy
one.
