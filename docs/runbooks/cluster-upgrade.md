# Cluster & Platform Upgrade — Runbook

**Applies to:** in-place upgrades of a running LLZ instance — the Kubernetes
version, the LKE-E node pools, the apl-core platform chart, and the first-party
LLZ charts.
**Source of truth:** the LandingZone spec (`environments/<env>.yaml`) →
rendered tfvars; the umbrella `vX.Y.Z` pin in the instance repo.

> This runbook is about **day-2 upgrades of an existing cluster**. For the
> LKE→LKE-E *migration* path see issue #37; for the **bootstrap "done" contract**
> the convergence gate enforces see
> [docs/architecture/convergence-contract.md](../architecture/convergence-contract.md).

---

## What can be upgraded, and how each is driven

| Layer | Declared in | Applied by | Who rolls it |
|---|---|---|---|
| **Kubernetes version** | `k8sVersion` (spec) → `k8s_version` in `cluster/<env>.tfvars` (e.g. `v1.33.6+lke7`) | `llz-terraform.yml` → `terraform apply` on the **cluster** root | LKE-E control plane (managed rolling node upgrade) |
| **Node pool** (size/type) | the pool block in the spec → cluster tfvars | same | LKE-E (drains + replaces nodes) |
| **apl-core platform** | `aplChartVersion` (spec) → `apl_chart_version` in `cluster-bootstrap/<env>.tfvars` (e.g. `5.0.0`) | `llz-terraform.yml` → `terraform apply` on the **cluster-bootstrap** root → Helm release upgrade | apl-operator's helmfile + Argo CD |
| **First-party LLZ charts** (`llz-*`) | `targetRevision` on each Argo CD Application (Renovate-bumped) | Argo CD | Argo CD rolling sync |
| **The `llz` CLI + reusable workflows + TF modules** | the umbrella `vX.Y.Z` pin | `llz upgrade` (copier) | n/a (re-render only) |

**Golden rule — promote, never jump straight to prod.** Environments are a
`dev → staging → prod` pipeline ([docs/environments-and-promotion.md](../environments-and-promotion.md)).
Every upgrade lands in the lowest-rank deployment first, soaks, then promotes by
raising the same field in the next env's spec. Per-env version drift is
expected and intended — a dev canary running ahead of prod is the whole point.

**Kubernetes upgrades are forward-only.** LKE-E does not support downgrading the
control plane. The rollback for a bad K8s bump is *roll forward* to a fixed
patch, or restore onto a fresh cluster (see [Rollback](#rollback)) — not a
version decrement. Bump **one minor at a time** (`1.32 → 1.33`, never
`1.31 → 1.33`).

---

## Preflight — before you change a version

Run these in order; do not proceed past a red check.

1. **Account capacity (Linode-side).** A node-pool upgrade transiently runs
   *extra* nodes (surge) and may need a spare VPC/vCPU headroom.
   ```bash
   LINODE_TOKEN=<token> llz ci preflight --region <region> \
     --vpc-limit <n> --vcpu-limit <n>
   ```
   Clears the same quota/orphan trap that stalls a fresh apply
   ([preflight](../../tools/cmd/llz/ci_preflight.go)); reap orphans first if it
   flags any.

2. **Cluster is converged *now*.** Don't upgrade a cluster that isn't already
   healthy — you won't be able to tell the upgrade from the pre-existing drift.
   ```bash
   llz status <env>          # nodes Ready, all Argo Applications Synced + Healthy
   llz drift <env>           # no unexpected drift vs the committed spec
   ```

3. **Secrets plane is healthy.** OpenBao must be **unsealed with a full Raft
   quorum** before any node churn — a node drain that takes a peer while another
   is already down loses quorum.
   ```bash
   kubectl -n llz-openbao get pods            # all 3 Ready
   # OpenBaoSealed / OpenBaoNoActiveLeader must NOT be firing (see docs/alerting.md)
   ```
   OpenBao is protected by a PodDisruptionBudget (`maxUnavailable: 1`) and
   required per-node anti-affinity, so a *rolling* drain keeps quorum — but only
   if it starts from 3/3 healthy. See [docs/resilience.md](../resilience.md).

4. **Take a backup / know your restore point.** Capture cluster state + the
   OpenBao recovery material before a platform upgrade (DR procedure:
   [docs/resilience.md](../resilience.md)). The static unseal key and Raft data
   are what make a restore possible if an apl-core upgrade corrupts state.

5. **Read the upstream changelog.** For an apl-core MAJOR bump, check its release
   notes for CRD migrations / breaking values changes; for a K8s bump, check for
   removed APIs your workloads still use (`kubectl` deprecation warnings, or a
   tool like `kubent`).

6. **Render is clean.** After editing the spec:
   ```bash
   llz render --check <env>   # the committed tfvars/overlay match the spec
   ```

---

## Upgrade procedures

### Kubernetes version (+ node pool rollout)

1. Bump `k8sVersion` in `environments/<env>.yaml` (one minor at most), `llz
   render <env>`, commit, open a PR.
2. Merge → `llz-terraform.yml` applies the **cluster** root. LKE-E performs a
   **rolling** node upgrade: each node is cordoned, drained (respecting PDBs —
   OpenBao stays at quorum), replaced at the new version, and rejoined.
3. Watch: `llz status <env>` until every node is `Ready` at the new version and
   all Argo Applications are `Synced + Healthy`. The convergence gate
   (`llz ci converge`) is the machine-checkable "done."
4. Soak, then promote to the next env.

### apl-core platform chart

1. Bump `aplChartVersion` in the spec, `llz render`, commit, PR.
2. Merge → `terraform apply` on **cluster-bootstrap** upgrades the Helm release;
   apl-operator's helmfile + Argo CD reconcile the new component versions in
   their wave order.
3. A MAJOR apl-core bump can ship CRD schema migrations — confirm the new CRDs
   established and no Application is stuck `Degraded` before promoting.

### First-party `llz-*` charts

Renovate raises a PR bumping the chart's `targetRevision`; Argo CD does the
rolling sync on merge. Each chart is immutable-by-version
([kubernetes-charts/README.md](../../kubernetes-charts/README.md)), so a bad
bump is reverted by pinning back to the prior `targetRevision`.

### The `llz` toolchain / umbrella pin

`llz upgrade` re-renders the instance's first-party pins to a new umbrella
`vX.Y.Z` (copier update). This changes *which* modules/workflows/CLI the instance
calls — it does not touch the cluster. Re-run the preflight + a `terraform plan`
afterward to see what the new module versions would change.

---

## Rollback

Match the rollback to the layer that broke:

| Broke during | Rollback |
|---|---|
| **apl-core chart upgrade** | Revert the `aplChartVersion` bump (git revert → `terraform apply`). Helm rolls the release back. **Caveat:** if the failed upgrade already ran an irreversible CRD/storage migration, a chart downgrade will not undo it — restore from backup instead. |
| **First-party `llz-*` chart** | Pin `targetRevision` back to the last-good version; Argo CD re-syncs. Tags are immutable, so the old version is intact. |
| **`llz upgrade` (umbrella pin)** | `git revert` the copier-update commit; the instance points back at the prior `vX.Y.Z`. No cluster change to undo. |
| **Kubernetes version** | **No downgrade.** Roll *forward* to a fixed patch of the same/next minor if one exists, or restore cluster state onto a freshly-provisioned cluster at a known-good version (DR: [docs/resilience.md](../resilience.md)). This is why K8s bumps go through dev/staging first. |
| **Node pool** | Revert the pool change in the spec → apply; LKE-E rolls the pool back to the prior shape. |

After any rollback: `llz status <env>` + `llz drift <env>` to confirm the
cluster is converged again, and capture what failed in the PR so the next
attempt carries the fix.

---

## Why this runbook exists

Before this, upgrades had no documented preflight, no "is it safe to drain
OpenBao right now" gate, and no written rollback path — so the first time an
apl-core MAJOR bump or a K8s minor went wrong in prod would be the first time
anyone reasoned about recovery. The promotion pipeline + the convergence gate
already make upgrades *observable*; this runbook makes them *reversible*.
