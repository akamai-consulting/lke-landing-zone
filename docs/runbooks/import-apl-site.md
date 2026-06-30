# Importing an existing APL site onto LLZ

**Commands:** `llz import scan` → `llz import init`
**Applies to:** adopting a pre-LLZ Akamai App Platform (APL/Otomi) cluster — inventory it, then scaffold a matching LLZ instance to migrate onto.

This is a **rebuild + migrate**, not an in-place upgrade: LLZ provisions a fresh LKE
cluster and installs apl-core via Helm, so importing means standing up a new
LLZ-managed instance configured to match the source and migrating onto it.

---

## The path

1. **`llz import scan`** — read-only inventory of the source site → `import-report.yaml`
2. **review** the report
3. **`llz import init`** — scaffold a new LLZ instance from the report (`llz new` + spec + `llz render`) and write `MIGRATION-TODO.md`
4. **`llz import plan`** — emit `MIGRATION-PLAN.md`, a runnable data-migration runbook (Object Storage + databases) from the report
5. **work the checklist + plan** — the steps a scan can't perform (see [Not yet covered](#not-yet-covered))

---

## 1. Scan

`llz import scan` inventories the source from up to four sources; each is optional
and best-effort (a missing kind/CRD warns and is skipped, never aborts):

| Source | Flag | Captures |
|---|---|---|
| Live cluster | `--kubeconfig` / `--context` | k8s version, region, node pools, storage classes, platform apps → components, operators (from CRDs), routing/domains (Istio + cert-manager), LoadBalancers, PVs (with Linode volume handles), databases, security posture, Helm releases, and a per-team breakdown (workloads, images, ingress hosts, PVCs/storage, secret names+types, ConfigMaps/SAs/RBAC, quotas) |
| APL platform-values | `--apl-values <file>` | The APL **DOWNLOAD PLATFORM VALUES** file — authoritative APL version, domain suffix, enabled/disabled apps, teams, object-store buckets + region, platform flags. **Config only — secret values are never read** |
| IaC / app repos | `--repo <dir>` (repeatable) | Walks a local clone with no assumed layout; inventories Terraform (resources/modules/providers/tfvars) + Kubernetes resources |
| Linode API | `--linode` (+ `LINODE_API_TOKEN`) | Provisioning detail kubectl can't see: node-pool **autoscaler** min/max, VPC subnet CIDR, Cloud Firewall inbound CIDRs, NodeBalancers, Object Storage buckets |

Where the sources disagree (declared vs running), the scan emits **drift** warnings
(e.g. an app declared disabled in APL but detected running).

```bash
# clone the source repos first (--repo / scan reads local clones, not URLs)
git clone <iac-repo-url> ./clones/app-repo

LINODE_API_TOKEN=<pat> llz import scan \
  --kubeconfig ./source.kubeconfig \
  --apl-values ./platform-values.yaml \
  --repo ./clones/app-repo \
  --linode \
  -o import-report.yaml
```

> **Security:** the downloaded platform-values file contains decrypted secrets (the
> SOPS age key, object-store keys, the admin password). `import scan` only reads
> config keys from it, but secure/delete the file when done and rotate if it isn't a
> disposable lab.

The scan ends by printing the exact `llz import init` command to run next.

## 2. Review the report

`import-report.yaml` (kind `ImportReport`) is the single inventory. Confirm the
cluster identity, the detected components, and the drift warnings before scaffolding.

## 3. Init

`llz import init` runs the **complete scaffold** from the report:

```bash
llz import init --report import-report.yaml --dir <instance-dir> --env <env>
```

1. `llz new` — copier scaffold (prompts for the instance identity the report can't supply: org / repo / forge)
2. authors `landingzone.yaml` + `environments/<env>.yaml` from the report — region, largest node pool → node_type/count, domain suffix, object-storage cluster, VPC subnet CIDR (if any)
3. sets the component toggles the scan found, then `llz render`s
4. writes `MIGRATION-TODO.md`

**Target versions, not the source's.** `init` renders the migration *target*: it
pins `apl_chart_version` to the chosen apl-core release and **leaves `k8s_version`
at the template default** (the source's k8s version isn't a valid LKE target) —
flagged in `MIGRATION-TODO.md` to set a valid `+lke` version for your account.

**Component granularity.** LLZ components are coarser than APL's per-app flags
(e.g. `observability` bundles prometheus + alertmanager + grafana + loki + otel).
A source that disabled an individual sub-app can't express that as a component
toggle — `MIGRATION-TODO.md` lists the source's disabled apps so you can re-disable
them via apl-values `_rawValues` if intended.

## 4. Work the checklist

`MIGRATION-TODO.md` enumerates what a scan can't carry over:

- **Decide by hand** — a valid `+lke` k8s version; `apiServerAllowCIDRs`; DNS cutover
- **Platform differences** — in-cluster Gitea → external Git, Tekton → Argo Workflows, Keycloak/IDP re-establishment
- **Secrets** — the per-team secret names to re-seed in OpenBao/ESO (values are never migrated)
- **Data** — PVs, databases, and object-storage buckets to migrate
- **Workloads** — per-team workload/image counts to redeploy

---

## 5. Data-migration plan

`llz import plan --report import-report.yaml` writes `MIGRATION-PLAN.md` — concrete,
copy-pasteable commands generated from the inventory:

- **Object Storage** — `rclone` remotes (source + target) and an incremental
  `rclone sync` per bucket; re-run as a final pass after the write freeze.
- **Databases** — per CNPG cluster, the **owning app** (from the DB-clients
  mapping) with its preferred app-native export/import, plus a CNPG-aware
  `pg_dump`/`pg_restore` fallback (same-version only).
- **Caches** (redis/valkey) — listed as rebuild-don't-migrate.

Target endpoints/credentials/bucket names are `${PLACEHOLDER}` env vars you fill.
The plan still does **not** cover block-PV contents (see below).

## Not yet covered

The import flow covers **configuration and inventory only**. It does **not** migrate
persistent state — PersistentVolume data, Object Storage bucket contents, and
database contents are surfaced in `MIGRATION-TODO.md` (with the Linode volume
handles, bucket list, and database list) but must be moved by hand for now.

Tracked in [akamai-consulting/lke-landing-zone#114](https://github.com/akamai-consulting/lke-landing-zone/issues/114).
