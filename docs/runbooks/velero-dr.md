# Velero Backup & Disaster-Recovery — Runbook

**Applies to:** an LLZ instance with the **`velero`** component enabled
(`spec.components.velero`). Velero backs up Kubernetes resources to a Linode
Object Storage bucket and snapshots PersistentVolumes via the Linode CSI driver.
**Component:** `instance-template/apl-values/components/velero/` (Argo Application
+ glue) · **TF:** the velero bucket + scoped key in
[`terraform-modules/llz-object-storage`](../../terraform-modules/llz-object-storage/).

> Closes the DR gap called out in the resilience posture: before Velero, a region
> loss was recoverable only to *infrastructure + GitOps state* (Terraform + Git +
> the OpenBao key material), **not** to point-in-time in-cluster data. Velero adds
> the in-cluster-data layer.

---

## What it backs up

| Layer | Mechanism | Where it lands |
|---|---|---|
| Kubernetes resources (Deployments, ConfigMaps, Secrets, CRs, …) | Velero backup metadata + resource manifests | the `platform-velero-<env>` Linode OBJ bucket (S3) |
| PersistentVolume **data** | **CSI VolumeSnapshots** via `linode-csi-velero` (driver `linodebs.csi.linode.com`) | Linode block-storage snapshots (**in-region**) |

**Schedule:** a `daily-backup` Schedule runs at 02:00, all namespaces, **30-day
retention** (`ttl: 720h`), `snapshotVolumes: true`.

> **Region-loss caveat (CSI snapshots are in-region).** CSI VolumeSnapshots live
> in the same Linode region as the cluster, so they do **not** survive a region
> loss — only the resource metadata in OBJ does. For cross-region PV-data DR,
> enable Velero's **CSI snapshot data movement** (`snapshotMoveData: true` on the
> schedule template + deploy the node-agent), which copies snapshot data into the
> OBJ bucket. That requires a privileged node-agent DaemonSet (an explicit
> deviation from the restricted-PSS posture) — adopt it deliberately. The default
> here favours the restricted-PSS posture + fast in-region restore.

## Prerequisites

- The cluster must have the **external-snapshotter** CRDs
  (`VolumeSnapshot`/`VolumeSnapshotContent`/`VolumeSnapshotClass`) + the
  snapshot-controller. LKE-E ships these with the Linode CSI driver.
- The `externalSecrets` component (Velero `DependsOn`) — the credentials arrive
  via OpenBao→ESO.

## Enabling Velero on a deployment

1. **Provision the bucket + key.** `velero` in `spec.components`, then
   `llz render <env>` writes the object-storage tfvars and the per-env
   BackupStorageLocation patch. `terraform apply` the `object-storage` root.
2. **Seed the credentials.** Extract the key and seed it (same flow as Loki/Harbor —
   see [linode-credential-rotation](linode-credential-rotation.md)):
   ```bash
   terraform output -raw velero_access_key   # → VELERO_S3_ACCESS_KEY (infra-<env>)
   terraform output -raw velero_secret_key   # → VELERO_S3_SECRET_KEY (infra-<env>)
   ```
   Then run `bootstrap-openbao.yml` for the region — `bao-seed-all` writes
   `secret/velero/object-store`, and the `velero-cloud-credentials` ExternalSecret
   syncs it into the cluster.
3. **Sync.** Argo CD deploys the `llz-velero` Application. Verify:
   ```bash
   kubectl -n velero get backupstoragelocation default   # PHASE: Available
   kubectl -n velero get pods                              # velero Running
   velero backup-location get                              # (with the velero CLI)
   ```

## Taking an on-demand backup

```bash
velero backup create pre-change-$(date +%s) --wait
velero backup describe <name> --details      # confirm "Phase: Completed", item counts
```

## Restore drill (do this BEFORE you need it)

A backup you have never restored is a hypothesis, not a backup. Run this drill on
a non-prod deployment each quarter:

1. **Pick a victim namespace** with a stateful workload (its own PVC).
2. **Back it up:**
   ```bash
   velero backup create drill --include-namespaces <ns> --wait
   ```
3. **Simulate loss:** `kubectl delete namespace <ns>` (or restore into a new
   namespace with `--namespace-mappings <ns>:<ns>-restore` to avoid touching the
   original).
4. **Restore:**
   ```bash
   velero restore create --from-backup drill --wait
   velero restore describe <name> --details
   ```
5. **Verify:** the pods come Ready, the PVC is bound to a volume restored from the
   CSI snapshot, and the application data is present. Record the wall-clock time —
   that is your measured **RTO** for this workload class.
6. **Clean up** the restore namespace.

Document the measured RTO/RPO in [docs/resilience.md](../resilience.md).

## RTO / RPO

| Scenario | RTO | RPO |
|---|---|---|
| Accidental namespace/resource deletion | minutes (`velero restore`) | ≤ 24h (since last daily backup; less if an on-demand backup was taken) |
| Single cluster rebuild (infra intact) | hours (Terraform + Argo + `velero restore`) | ≤ 24h |
| **Region loss** | hours–day (rebuild in a new region) | **resource metadata: ≤ 24h; PV data: unbounded** unless CSI data movement is enabled (see caveat) |

## Backup health monitoring

Velero exposes Prometheus metrics (the `metrics.serviceMonitor` is enabled);
kube-prometheus-stack scrapes them. The signals to alert on:

- `velero_backup_last_successful_timestamp` — alert if the newest successful
  backup is older than ~26h (a missed daily run).
- `velero_backup_failure_total` / `velero_volume_snapshot_failure_total`
  increasing.

> A `PrometheusRule` for these (and a scheduled-checks belt-and-suspenders that
> fires even if Prometheus is down) is the natural follow-up — see the alerting
> inventory ([docs/alerting.md](../alerting.md)). Until then, check
> `velero backup get` (newest `COMPLETED`) during routine ops.

## Rotation

The velero OBJ key rotates on the same 120-day clock as the Loki/Harbor keys
(`time_rotating.loki_key`). On rotation, reseed `VELERO_S3_ACCESS_KEY` /
`VELERO_S3_SECRET_KEY` and re-run `bootstrap-openbao.yml` — identical to the
Loki/Harbor reseed in [linode-credential-rotation](linode-credential-rotation.md).
