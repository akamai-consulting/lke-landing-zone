# Resilience & Disaster Recovery

What survives a node loss, a zone blip, or a cluster rebuild — and what an
operator must do to recover. This page is the posture; the step-by-step upgrade
flow is the [cluster-upgrade runbook](runbooks/cluster-upgrade.md).

## Availability posture (voluntary disruption: node upgrades, autoscaler)

The one piece of state whose availability is *quorum-critical* is the secrets
plane, and it is protected:

| Component | Replicas | Disruption protection | Spread |
|---|---|---|---|
| **OpenBao** (Raft) | 3 | **PodDisruptionBudget `maxUnavailable: 1`** (from the upstream chart, HA mode) — a rolling node drain can take at most one peer, so the 2-of-3 Raft quorum holds | **required** pod-anti-affinity on `kubernetes.io/hostname` — one peer per node |
| LKE-E control plane | managed | Linode-managed dedicated HA control plane | n/a |
| apl-core support plane (Prometheus, Grafana, Loki, Harbor, OTel) | **1 each** (apl-core defaults) | none — see below | none |

### Why we do *not* add PodDisruptionBudgets to the apl-core components

It's tempting to "add PDBs everywhere," but on the single-replica apl-core
support components a PDB is **actively harmful**: a `minAvailable: 1` (or
`maxUnavailable: 0`) budget on a 1-replica Deployment makes the lone pod
*undrainable* — a node upgrade or autoscaler scale-down then **hangs forever**
waiting for a disruption the budget will never allow. The correct lever for
those components is **horizontal scale (≥2 replicas) + topology spread**, which
is an apl-core values + resource-cost decision, not a safe drop-in PDB. Until a
component is genuinely run HA, it relies on fast reschedule after eviction.

So the rule encoded here: **PDBs only where replicas ≥ 2 and the workload is
quorum- or availability-critical.** Today that's OpenBao (handled upstream), and
it's why this repo ships no additional PDBs.

### Topology

- Run the node pool with **≥3 nodes** so OpenBao's required anti-affinity can
  place one peer per node (with <3 schedulable nodes a peer stays `Pending`).
- A **single region** per cluster. Cross-region/-cluster failover is **not**
  provided — the resilience boundary is the region. Surviving a region loss is a
  *restore onto a new cluster* operation (below), not an automatic failover.

## Disaster-recovery posture (involuntary loss: cluster/region gone)

### What is already recoverable

| Asset | Where it lives off-cluster | Restore path |
|---|---|---|
| **OpenBao unseal (static) key** | `infra-<region>` GitHub Environment secret, seeded by `llz ci bao-seed-seal-key` | re-mounted by the chart on a fresh cluster; pods self-unseal |
| **OpenBao recovery keys** | `infra-<region>` GitHub Environment secrets | `bao operator generate-root` to regain root after restore ([docs/secrets.md](secrets.md)) |
| **Terraform state** | Linode Object Storage (encrypted) | `terraform apply` from the instance repo re-provisions infra |
| **All cluster manifests / Argo Applications / apl-values** | the instance Git repo | Argo CD re-syncs them onto a fresh cluster |
| **Object-storage data** (Harbor registry, Loki chunks) | Linode OBJ buckets (outlive the cluster) | re-referenced by the rebuilt cluster |

A cluster can therefore be **rebuilt from Terraform + Git + the GitHub-Environment
key material** — the platform is largely reconstructable.

### What is NOT yet backed up (the gap)

- **In-cluster Kubernetes resources that aren't in Git** — anything created
  imperatively, plus CRD instances and Secrets that ESO/controllers materialise
  at runtime.
- **PersistentVolume data** — OpenBao's Raft volume (the secrets themselves, not
  just the unseal key), and any stateful app PVCs. Linode disk encryption
  protects these at rest but does **not** snapshot or replicate them.

There is **no cluster-state / PV backup tool (e.g. Velero) deployed today.** A
region loss is recoverable to *infrastructure + GitOps state*, but **not to
point-in-time in-cluster data**. Closing this is tracked as the backup/restore
work (issue #6 / gap-analysis item C1): a Velero deployment backing cluster
resources + PV contents to Linode Object Storage, with a documented restore
drill. **Until that lands, treat OpenBao's Raft data as recoverable only via the
recovery keys, and stateful app data as un-backed-up.**

### RTO / RPO targets (proposed baseline)

These are the targets the backup work (C1) should be designed to meet; document
the *measured* values once Velero + a restore drill exist.

| Scenario | RTO (time to restore) | RPO (data loss window) |
|---|---|---|
| Single node / pod loss | seconds–minutes (auto-reschedule; OpenBao quorum holds) | 0 |
| Single cluster rebuild (infra intact) | hours (Terraform re-apply + Argo re-sync + OpenBao restore) | up to last Git commit + last backup |
| Region loss | hours–day (new region, full rebuild) | last off-region backup (**today: only what's in Git/OBJ/GH-env**) |

The RPO for in-cluster/PV data is **unbounded today** (no backup) — that is the
single most important reason to land C1.

## See also

- [cluster-upgrade runbook](runbooks/cluster-upgrade.md) — voluntary-disruption (upgrade) safety
- [docs/secrets.md](secrets.md) — OpenBao seal/recovery key handling
- [docs/alerting.md](alerting.md) — what signals a disruption (OpenBaoSealed, NoActiveLeader, scrape-down)
- [bootstrap-openbao runbook](runbooks/bootstrap-openbao.md) — re-seeding the secrets plane on a fresh cluster
