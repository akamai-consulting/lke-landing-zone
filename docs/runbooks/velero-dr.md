# Velero backup and restore

**Applies to:** a deployment with the **`velero`** component enabled
(`spec.components.velero`, off by default) whose workload state must come back from a
backup; or `LLZVeleroBackupFailing` / `LLZVeleroBackupStale` /
`LLZVeleroBackupNeverSucceeded` / `LLZVeleroStorageUnavailable` /
`LLZVeleroMetricsAbsent` is firing; or you are deciding whether to enable it.
**Component:** [`platform-apl/components/velero/`](https://github.com/akamai-consulting/lke-landing-zone/tree/main/platform-apl/components/velero)
· **Bucket + key:** [`terraform-modules/llz-object-storage`](https://github.com/akamai-consulting/lke-landing-zone/tree/main/terraform-modules/llz-object-storage)
· **Gate:** `llz ci assert-velero-restore`.

> **OpenBao is never restored from Velero.** `llz-openbao` is excluded from every
> Velero backup, and `llz render` cannot be told otherwise: its `openbao-unseal-key`
> Secret *is* `OPENBAO_SEAL_KEY`, and a bucket holding the seal key beside the Raft
> store it unseals would be the whole of OpenBao to anyone who can read it. OpenBao has
> its own backups — restore it from those
> ([openbao-backup-restore](openbao-backup-restore.md)), and do that **first**: the key
> Velero's backups are encrypted under lives in OpenBao.
>
> **Never lose `secret/obj/velero-ssec`.** Every backup object is SSE-C under that key
> and Linode keeps no copy. It is in OpenBao (and therefore in OpenBao's snapshots);
> escrow it offline as well, exactly like the objProxy key.

<!-- toc -->
## Contents

- [What Velero is for here](#what-velero-is-for-here)
- [What is backed up](#what-is-backed-up)
- [What is not backed up](#what-is-not-backed-up)
- [Confidentiality](#confidentiality)
- [Enabling it](#enabling-it)
- [An on-demand backup](#an-on-demand-backup)
- [Restoring: what to include, and what never to](#restoring-what-to-include-and-what-never-to)
- [Restore after the cluster was lost](#restore-after-the-cluster-was-lost)
- [Restore drill](#restore-drill)
- [RTO and RPO](#rto-and-rpo)
- [The alerts](#the-alerts)
- [Keys, rotation, and destroying the bucket](#keys-rotation-and-destroying-the-bucket)

<!-- /toc -->

## What Velero is for here

The platform's own state is not Velero's job. It comes back from elsewhere:

| State | Comes back from |
|---|---|
| Cluster, network, buckets | Terraform (the `object-storage` root's buckets survive a cluster destroy) |
| Platform configuration — apl-core values, LLZ components | Git (the instance repo and the `apl-<env>` values branch) |
| Every platform secret | OpenBao, restored from its own Raft snapshots ([openbao-backup-restore](openbao-backup-restore.md)) |
| apl-core's CNPG databases (Harbor, Gitea, Keycloak) and Gitea repositories | apl-core's `platformBackups` — configured in the App Platform Console, not by LLZ |

apl-core ships no Velero. What this component adds is **point-in-time Kubernetes
resource state for the workloads you run** — the Deployments, ConfigMaps, Secrets and
custom resources in team and application namespaces that are not in Git, or not only in
Git. That is a choice per deployment, which is why it is off by default: it puts a copy
of every included Secret in a bucket.

## What is backed up

One Schedule, `velero-cluster` in `llz-velero`, writes to
`<objLabelPrefix>-velero-<deployment>`:

| Knob (`spec.components.velero`) | Default | Rule |
|---|---|---|
| `backupIntervalHours` | `24` (daily at 01:37 UTC) | a divisor of 24 (`37 */N * * *`) |
| `retention` | `30d` (TTL `720h`) | at least two intervals and a day; at most a year |
| `backupIncludeNamespaces` | `*` | space-separated names or Velero globs; never `llz-openbao` / `llz-velero`; `*` only alone |
| `backupExcludeNamespaces` | see below | space-separated; **replaces** the default list; never `*` |

`llz render` writes them into `apl-values/<env>/apps/velero/velero-values-patch.yaml`,
defaults included, so the rendered tree shows what the cluster runs.

**Always excluded**, whatever the spec says: `llz-openbao` (the seal key and the store)
and `llz-velero` (Velero's own key, and the SSE-C key its backups are encrypted under).

**Excluded by default** (replace the list to change it): `kube-system`, `kube-public`,
`kube-node-lease` (LKE-E's, recreated by the provider); `cert-manager` (the
`llz-client-ca` and `openbao-bootstrap-ca` private keys — a copy is a way to mint client
certificates OpenBao trusts); `istio-system` (the mesh CA key); `external-secrets` (ESO's
mTLS identity to OpenBao); `llz-openbao-backup`, `llz-pat-rotator`, `llz-reconciler`,
`obj-proxy` (LLZ workloads whose only state is credentials from OpenBao and
GitOps-rendered objects).

Each backup holds the objects of every included namespace — **Secrets included** — and
the cluster-scoped objects Velero ties to them (the PersistentVolume bound to an
included PVC). `includeClusterResources` is never set: with any namespace excluded,
Velero then takes only *related* cluster objects, which is what keeps `llz-openbao`'s
PersistentVolumes out along with its PVCs. `TestVeleroScheduleNeverIncludesOpenBao`
holds the base manifest and every rendered patch to all of this, and the gate re-checks
the **live** Schedule on every bootstrap.

## What is not backed up

**Volume data.** The component takes no volume snapshots and runs no node-agent:

- Linode Block Storage gives the CSI driver no snapshot API to drive, so CSI
  `VolumeSnapshot`s cannot be relied on here.
- Even if they could, this platform **denies** snapshot- and clone-sourced PVCs at
  admission (the `pvc-deny-untaggable-clone` ValidatingAdmissionPolicy): the clone path
  cannot tag the new Volume with its cluster, so `llz reap` could never clean it up.
- File-system backup (kopia) needs a privileged node-agent DaemonSet with `hostPath`
  access to every pod's volumes, which restricted Pod Security refuses. Enabling it is
  an owner decision, not a default.

What a restore does with a PVC and its PV: the objects come back. A PV on
`block-storage-retain` (reclaim policy Retain) is restored **as-is**, pointing at the
same Linode Volume — so in the same region, after the cluster was lost but the Volume
survived (Retain), the data reattaches. If the Volume is gone, the pod will not start
and the data is not in the bucket. **Region loss loses PV data.**

**Also not backed up:** cluster-scoped objects unrelated to an included namespace (CRDs,
ClusterRoles, StorageClasses — GitOps owns them), and anything in an excluded namespace.

## Confidentiality

- **SSE-C, by Velero itself.** The AWS plugin sends the SSE-C headers on every object it
  writes and reads (`customerKeyEncryptionSecret: velero-ssec-key/customer-key`). The
  32-byte key is generated once by `llz ci seed-ssec-key` into `secret/obj/velero-ssec`
  and projected into `llz-velero` by ESO. Linode encrypts each object under it and
  discards the key, so the bucket holds ciphertext and a holder of the bucket key alone
  reads nothing. This is the same mode the objProxy gateway gives Loki and Harbor
  ([obj-sse-c-gateway](../designs/obj-sse-c-gateway.md)), applied natively — Velero can
  emit SSE-C, Loki and Harbor cannot — so backups do not depend on that default-off
  gateway being in the write path.
- **Virtual-hosted addressing** (`s3ForcePathStyle: "false"`), so the objProxy CoreDNS
  rewrite of the bare endpoint host never captures Velero's traffic.
- **A key scoped to this bucket alone** (`<prefix>-velero-<deployment>`, read_write),
  never the broad platform key apl-core hands to Loki and Harbor.
- **The cost of SSE-C you will notice:** `velero backup logs`, `velero restore logs` and
  `velero backup describe --details` download through presigned URLs, which an SSE-C
  object refuses without the key headers. Read outcomes off the CRs instead:

  ```bash
  kubectl -n llz-velero get backups --sort-by=.metadata.creationTimestamp
  kubectl -n llz-velero get backup <name> -o jsonpath='{.status}'
  kubectl -n llz-velero logs deploy/velero | grep <name>
  ```

## Enabling it

1. `components: { velero: { enabled: true } }` in `environments/<deployment>.yaml`, then
   `llz render <deployment>` and commit.
2. Apply the `object-storage` root if this deployment predates the velero bucket.
3. Re-run `bootstrap-openbao` for the deployment. It mints Velero's key
   (`mint-bootstrap-objkeys`, now that the component is on), generates the SSE-C key
   (`seed-ssec-key` — **escrow it offline when it prints**), and, after converge, runs
   `llz ci assert-velero-restore --region <deployment>`: the first backup, read back from
   the bucket and proven encrypted. Until both keys exist the ExternalSecrets fail and
   the Velero server never starts.
4. Verify:

   ```bash
   kubectl -n llz-velero get backupstoragelocation default   # PHASE: Available
   kubectl -n llz-velero get schedule velero-cluster
   kubectl -n llz-velero get backups                         # velero-cluster-check-<ts>: Completed
   ```

## An on-demand backup

Take it **from the Schedule**, which carries the exclusions:

```bash
velero -n llz-velero backup create pre-change-$(date +%s) --from-schedule velero-cluster --wait
```

A bare `velero backup create` does **not** inherit them. If you must scope one by hand,
pass `--exclude-namespaces llz-openbao,llz-velero` yourself — and never
`--include-cluster-resources=true`, which backs up `llz-openbao`'s volumes despite the
namespace exclusion.

## Restoring: what to include, and what never to

Restore **workload namespaces, named explicitly**, and leave everything the platform
re-creates to the platform:

```bash
velero -n llz-velero restore create --from-backup <backup> \
  --include-namespaces team-payments,orders \
  --include-cluster-resources=false \
  --existing-resource-policy=none \
  --wait
kubectl -n llz-velero get restore <restore> -o jsonpath='{.status}'
```

- `--existing-resource-policy=none` (Velero's default) skips anything that exists, so a
  restore never overwrites a live object. Use `update` only for a namespace you have
  emptied on purpose.
- `--include-cluster-resources=false` keeps the restore to namespaced objects. Drop it
  only to bring back PVs deliberately (see [What is not backed up](#what-is-not-backed-up)).
- **apl-core team namespaces** (`team-<name>`): create or keep the team in apl-core
  first (the console or apl-api provisions the namespace, its RBAC and the Keycloak
  group), then restore the team's objects into it. The namespace itself already exists,
  so `none` skips it.

**Never restore**, even though some are in the backups:

| Namespace | Why |
|---|---|
| `llz-openbao` | not in any backup, by design; restore it from its own snapshots |
| `argocd` and the apl-core namespaces (`keycloak`, `gitea`, `harbor`, `monitoring`, `istio-system`, `cert-manager`, `external-secrets`, `apl-*`, `otomi`, …) | apl-core and Argo CD own them on the managed App Platform. A restored object races its owner: Argo prunes or overwrites it, apl-core re-renders it, and a restored Application or Secret can point the platform back at a revoked credential |
| `llz-*`, `obj-proxy` | LLZ components, re-created by `llz render` + Argo; their credentials come from OpenBao |

ESO-managed Secrets in a restored namespace are rewritten by ESO from OpenBao within
their refresh interval, which is what you want: the backup's copy may be a credential
rotated since.

## Restore after the cluster was lost

1. **Rebuild** the cluster with Terraform. Do not destroy `object-storage` — the velero
   and openbao-backup buckets are what you are restoring from (the destroy refuses while
   they hold anything, unless `drain_data_buckets=true`).
2. **Restore OpenBao from its Raft snapshot** first
   ([openbao-backup-restore, rebuild](openbao-backup-restore.md#restore-the-cluster-was-lost-and-rebuilt)).
   That brings back `secret/obj/velero-ssec`; without it every Velero backup is
   unreadable. Then follow its "After a restore": the restored `secret/obj/velero` may
   name a key since revoked — re-run `mint-bootstrap-objkeys --reseed` if the BSL stays
   Unavailable.
3. Let GitOps converge the platform, `velero` included. Velero lists the bucket and
   recreates the Backup objects within a minute of the BSL going Available:
   `kubectl -n llz-velero get backups`.
4. **Restore workload namespaces** as above, newest backup first.

## Restore drill

`llz ci assert-velero-restore` runs on every bootstrap of a deployment with the
component on: the first backup, read back from the bucket and proven SSE-C. On the
release-e2e deployment it also runs with `--drill` (the `restore_drill` input): it writes
a ConfigMap and a Secret holding a nonce into `llz-velero-drill`, backs that namespace up
through the Schedule's template, **deletes it**, restores it, and fails unless both come
back with the nonce and the restore's own label. It touches no other namespace and is
refused on any deployment that is not an e2e lane.

Do the same by hand on a non-production deployment each quarter, with a namespace that
matters to you:

```bash
velero -n llz-velero backup create drill-$(date +%s) --from-schedule velero-cluster --wait
kubectl delete namespace <ns>          # or restore elsewhere: --namespace-mappings <ns>:<ns>-drill
velero -n llz-velero restore create --from-backup <backup> --include-namespaces <ns> --wait
```

Record the wall-clock time from delete to Ready — that is this workload class's RTO.

## RTO and RPO

| Scenario | RTO | RPO |
|---|---|---|
| A namespace or objects deleted by mistake | minutes (one `velero restore`) | up to one backup interval (24h by default), less after an on-demand backup |
| Cluster lost, region intact | hours: Terraform, OpenBao restore, converge, then `velero restore` | resources: one interval. PV data: none lost while its Retain Volume survives |
| Region lost | hours to a day: rebuild elsewhere, same order | resources: one interval, **if** the bucket's region survived. PV data: **lost** |

## The alerts

On Velero's own metrics for `schedule="velero-cluster"`
([prometheusrule.yaml](https://github.com/akamai-consulting/lke-landing-zone/blob/main/platform-apl/components/velero/velero/prometheusrule.yaml)):

| Alert | Fires when | First check |
|---|---|---|
| `LLZVeleroBackupFailing` (warning) | the newest backup ended Failed / PartiallyFailed | `kubectl -n llz-velero get backup <newest> -o jsonpath='{.status}'` |
| `LLZVeleroBackupStale` (critical) | no successful backup for 49h (two runs of the longest interval + 1h) | is the server running; is the Schedule paused |
| `LLZVeleroBackupNeverSucceeded` (warning) | the Schedule has existed 26h with no success | the BSL, and the two Secrets in `llz-velero` |
| `LLZVeleroStorageUnavailable` (warning) | the BSL cannot list the bucket | `kubectl -n llz-velero get bsl default -o yaml` (`status.message`): a revoked key, a missing bucket |
| `LLZVeleroMetricsAbsent` (warning) | no Velero series at all — every other alert is blind | the Deployment, the ServiceMonitor's `prometheus: system` label, the `:8085` ingress from `monitoring` |

## Keys, rotation, and destroying the bucket

- **The object-storage key** (`secret/obj/velero`) rotates on the in-cluster rotator's
  cadence, like the other object-storage keys
  ([linode-credential-rotation](linode-credential-rotation.md)). ESO refreshes
  `velero-cloud-credentials` within five minutes, and the Secret is a volume mount, so the
  file changes under the running server. If the BSL goes Unavailable right after a
  rotation, `kubectl -n llz-velero rollout restart deploy/velero`.
- **The SSE-C key** (`secret/obj/velero-ssec`) is generate-once and never rotated:
  SSE-C is per object, so a new key over the bucket makes every existing backup
  unreadable. `seed-ssec-key` leaves an existing key alone.
- **The bucket is versioned**: a backup deleted by Velero's GC or by hand stays
  recoverable as noncurrent versions for `velero_noncurrent_days` (default 14). That
  does not protect against a holder of the velero key or `LINODE_API_TOKEN` deleting
  versions on purpose.
- **Destroying `object-storage`** refuses while the velero or openbao-backup bucket
  holds any version, unless the dispatch sets `drain_data_buckets=true`
  (`llz ci openbao-backup-destroy-guard` guards both).
- **Disabling the component** deletes the Velero Application, its CRDs and the Backup
  objects in the cluster — **not** the bucket's contents. Re-enable it and Velero lists
  the bucket again.
