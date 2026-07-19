# Runbook — in-cluster reconciler, convergence & support-plane alerts

Operational response for the platform's in-cluster observability alerts — the
ones the **llz-reconciler** raises about itself and the cluster, plus the
support-plane scrape/availability alerts. Every alert below carries a
`runbook_url` annotation pointing here.

**First stop for all of these:** the **LLZ Day-2** Grafana dashboard
(`uid: llz-day2`) — it shows convergence state, per-reconciler up/errors/staleness,
ESO/cert readiness, OpenBao seal, and credential age at a glance. Most of these
alerts are diagnosed by looking at *which* reconciler or service is red there.

Preflight for anything cluster-side:

```bash
llz ci fetch-kubeconfig --region <env>     # or the cluster-access action
kubectl -n llz-reconciler get pods
kubectl -n llz-reconciler logs deploy/llz-reconciler --tail=200
```

---

## Reconciler self-health

| Alert | Means | Do |
|-------|-------|----|
| `LLZReconcilerScrapeDown` | Prometheus can't scrape `:8080/metrics`. | Check the pod is Running + Ready; check the ServiceMonitor + the `monitoring→llz-reconciler` NetworkPolicy. If the pod is up but unscraped, the scrape path (Service/ServiceMonitor/NetworkPolicy) is the fault. |
| `LLZReconcilerReportingDown` | Scraped, but `llz_reconcile_up == 0` for a reconciler — its last pass failed. | `kubectl logs` the pod; the error is logged (lost Kubernetes API access, an RBAC 403 after a grant change, or a rotation/provisioning error). If it's a 403, the reconciler's RBAC was over-tightened — check the Role for that reconciler's namespace. |
| `LLZReconcilerStale` | **Any** lane with no successful pass in 10× its own interval (`llz_reconcile_interval_seconds`) — for `observe` (30s) that is the > 5m this alert always meant. | The named lane's loop is wedged or the API is unreachable; restart the Deployment if logs show a hung client. **Treat every gauge that lane publishes as fiction until this clears** — the registry never expires a sample, so its metrics still read at their last-known-good value and the alerts fed by them (e.g. `LLZOpenBaoSealed` from `openbao-gauges`) cannot fire. |
| `LLZReconcilerErroring` | > 3 reconcile errors in 1h from a driving reconciler (Linode cred rotation / Harbor provisioning / cidr-firewall). | Logs name the failing reconciler + the underlying error (Linode API, OpenBao login, ConfigMap patch). |
| `LLZReconcilerNoLeader` | A driving reconciler is enabled but no replica holds the lease. | Check the `llz-reconciler-*` Lease in the `llz-reconciler` namespace + the leader-election RBAC. Single-replica by design, so this usually means the pod is down. |

## Cluster convergence

| Alert | Means | Do |
|-------|-------|----|
| `LLZClusterNotConverged` | `llz_convergence_state == 1` (hard-failed) — the same verdict `llz ci health` gives. | Run `llz ci health` for the report (or check `llz_convergence_apps_failed` on the dashboard for the failing Argo apps). This is the in-cluster mirror of the bootstrap converge gate — see [bootstrap-openbao.md](bootstrap-openbao.md) for the wedge classes (sync-wave, ESO timing). |
| `LLZConvergenceUnobserved` | `llz_convergence_apps_observed == 0` — the convergence verdict was computed from **no Argo Applications at all**. State 0 ("converged") here means "nothing was examined", not "everything is healthy". | Pre-bootstrap this is expected and clears when the Applications are created. Otherwise the list read is returning nothing: check the reconciler's RBAC on `applications.argoproj.io` in `argocd`, that the namespace is still `argocd`, and `kubectl logs` for parse failures. |
| `LLZConvergenceSignalAbsent` | No `llz_convergence_state` series exists at all — the `observe` lane is not publishing it. | The convergence signal is unmonitored (not healthy): `LLZClusterNotConverged` and `LLZConvergenceUnobserved` are both thresholds over a series that isn't there, so neither can fire. Check the `observe` lane in the pod logs and `llz_reconcile_up{reconciler="observe"}`. |
| `LLZESOStoreNotReady` | The `openbao` ClusterSecretStore is not Ready — ESO can't resolve any ExternalSecret. | Almost always OpenBao: check it's unsealed + reachable ([bootstrap-openbao.md](bootstrap-openbao.md)). Every ExternalSecret in the cluster is stalled until this clears. |
| `LLZCertificatesNotReady` | One or more cert-manager Certificates stuck `Ready=False`. | `kubectl get certificate -A | grep -v True`. A stuck ACME cert (e.g. `otel.<env>.internal`) usually means a DNS-01 challenge failure; the deferred llz-letsencrypt issuers are expected NotReady until `spec.dns.acmeEmail` is set. |

## Support plane (apl-core services)

| Alert | Means | Do |
|-------|-------|----|
| `OTelCollectorMetricsTargetDown` / `LokiMetricsTargetDown` / `GrafanaMetricsTargetDown` / `HarborMetricsTargetDown` | Prometheus can't scrape the service — could be a down pod OR a broken metrics endpoint on a healthy pod. | `kubectl get pods -n <service-ns>`; if pods are Ready, inspect the metrics endpoint / ServiceMonitor. |
| `SupportPlaneDeploymentUnavailable` | A load-bearing Deployment (harbor-core/registry, grafana, otel-collector) has **0 available replicas** — actually down, not just unscrapable. | `kubectl describe deploy/<name> -n <ns>` + pod events (image pull, OOM, scheduling, a failing ExternalSecret it mounts). |
| `LokiStatefulSetUnavailable` | The Loki StatefulSet has 0 ready replicas — log ingestion + queries are down. | `kubectl -n monitoring describe statefulset loki` + `logs loki-0`; a common cause is the S3 object-store credential (`secret/loki/object-store`) not synced. |

OpenBao's own alerts (`OpenBaoSealed`, `OpenBaoNoActiveLeader`,
`OpenBaoRaftQuorumDegraded`, …) and the `LLZOpenBao*` reconciler mirrors are in
[bootstrap-openbao.md](bootstrap-openbao.md); `LLZCredentialRotationOverdue` is in
[linode-credential-rotation.md](linode-credential-rotation.md).
