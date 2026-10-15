# CNPG database backup and restore

**Applies to:** a deployment with the **`cnpgBackup`** component enabled
(`spec.components.cnpgBackup`, off by default) whose Keycloak or Harbor database must
come back from a backup; or `LLZCnpgBackupFailing` / `LLZCnpgBackupStale` /
`LLZCnpgBackupNeverSucceeded` / `LLZCnpgWalArchivingFailing` /
`LLZCnpgBackupMetricsAbsent` is firing; or you are deciding whether to enable it.
**Component:** [`platform-apl/components/cnpgBackup/`](https://github.com/akamai-consulting/lke-landing-zone/tree/main/platform-apl/components/cnpgBackup)
(the alerts) and `apl-values/<env>/apl-overlay/platformBackups.yaml` (what `llz render`
tells apl-core)
· **Bucket:** [`terraform-modules/llz-object-storage`](https://github.com/akamai-consulting/lke-landing-zone/tree/main/terraform-modules/llz-object-storage)
· **Gate:** `llz ci assert-cnpg-backup`.

> **These backups are apl-core's, and so is the restore.** LLZ runs no backup code
> here: it switches on apl-core's NATIVE `platformBackups` for the two databases and
> points them at this deployment's `cnpg` bucket. apl-core's otomi-db chart then renders
> a barman-cloud `ObjectStore`, the WAL-archiving plugin on the CloudNativePG `Cluster`,
> and a `ScheduledBackup`. A restore is apl-core's `databases.<db>.recovery` — see
> [Restoring a database](#restoring-a-database).
>
> **The bucket's key is shared.** apl-core gives every app ONE object-storage
> credential, so the platform key Loki and Harbor use can read and delete these
> backups. Versioning is the mitigation — see [The shared key](#the-shared-key).

<!-- toc -->
## Contents

- [What is backed up](#what-is-backed-up)
- [The shared key](#the-shared-key)
- [Encryption at rest](#encryption-at-rest)
- [Enabling it](#enabling-it)
- [What the bootstrap gate proves](#what-the-bootstrap-gate-proves)
- [Restoring a database](#restoring-a-database)
- [Restore after the cluster was lost](#restore-after-the-cluster-was-lost)
- [Restore drill](#restore-drill)
- [RTO and RPO](#rto-and-rpo)
- [The alerts](#the-alerts)
- [Disabling it, and destroying the bucket](#disabling-it-and-destroying-the-bucket)

<!-- /toc -->
## What is backed up

| Database | apl-core Cluster | Namespace | Holds |
|---|---|---|---|
| `keycloak` | `keycloak-db` (release `keycloak-otomi-db`) | `keycloak` | every realm, user, credential hash, client secret and realm signing key |
| `harbor` | `harbor-otomi-db` | `harbor` | Harbor's metadata: projects, repositories, robot accounts, scan results. **Not** the image layers — those are in the harbor-registry bucket |

`harbor` is backed up only where the `harbor` component runs (and, on managed, `harbor`
is in `bootstrap.managedApps`); the default set drops it elsewhere. Gitea is never
backed up: LLZ disables apl-core's gitea on every cluster.

Each database gets a barman **base backup** on the schedule plus **continuous WAL
archiving** in between, under
`s3://<prefix>-cnpg-<env>/<db>-<generation>/<cluster>/{base,wals}/`. Knobs
(`components.cnpgBackup`, all validated — see [landing-zone-spec](../landing-zone-spec.md)):

| Knob | Default | Meaning |
|---|---|---|
| `backupIntervalHours` | `24` (daily at 01:07 UTC) | base backup cadence, a divisor of 24 |
| `retention` | `30d` | barman's `retentionPolicy`, in `d`/`w`/`m` (`m` is **months**); at least two intervals, at most a year |
| `backupDatabases` | `harbor keycloak` | which databases; `gitea` is refused |
| `backupGeneration` | `1` | the `<db>-<generation>` prefix — see [Restoring a database](#restoring-a-database) |

## The shared key

apl-core's chart builds barman's credential (the `linode-creds` Secret in each database
namespace) from the same `obj.provider.linode.accessKeyId` and `obj-secrets` pair every
apl-core app reads. There is no per-app object-storage key to point at a dedicated one —
the answer the OpenBao and Velero buckets take — so the cnpg bucket is in the scope of
the **platform key** (`<prefix>-obj-<env>`, rotation-table entry `obj-platform`).

**Consequence:** Loki, Harbor, and anything able to read `obj-secrets` can read and
delete the database backups.

**Mitigation:** the bucket is versioned and keeps noncurrent versions for
`cnpg_noncurrent_days` (default **30**, twice velero's because the key is shared). A
delete or overwrite — a Loki or Harbor bug, a compromised pod, a retention mistake —
stays recoverable for that window:

```bash
# list what a delete left behind (needs a key on the bucket; see Restore after the cluster was lost)
aws --endpoint-url https://<cluster>.linodeobjects.com s3api list-object-versions \
  --bucket <prefix>-cnpg-<env> --prefix keycloak-1/
# restore one by copying the noncurrent version over the delete marker
aws --endpoint-url https://<cluster>.linodeobjects.com s3api copy-object \
  --bucket <prefix>-cnpg-<env> --key <key> --copy-source <prefix>-cnpg-<env>/<key>?versionId=<id>
```

Versioning does **not** protect against a key holder deleting versions on purpose;
Linode has no object lock. If that is in your threat model, also take periodic
`pg_dump`s to storage the platform key cannot reach.

## Encryption at rest

barman-cloud is boto3, and botocore forces **path-style** addressing whenever an
endpoint URL is given — so barman's requests go to `https://<cluster>.linodeobjects.com/<bucket>/…`,
the exact host the objProxy CoreDNS rewrite captures.

- **objProxy on:** the backups go through the SSE-C gateway and are **encrypted** under
  `secret/obj/ssec`. The objProxy component issues its CA into `harbor` and `keycloak`
  and its Kyverno policy `cnpg-obj-proxy-ca` sets the plugin's native `endpointCA` on
  apl-core's ObjectStores, so barman trusts the proxy. Reading them needs the proxy and
  that key — after a loss, OpenBao is restored first.
- **objProxy off:** the objects are **plaintext at rest** — apl-core exposes no SSE-C
  setting for barman. The bucket is registered with the at-rest guard for that reason.

**Do not toggle objProxy under a live archive without bumping `backupGeneration`.** An
archive written half plaintext and half SSE-C cannot be read through either path in full.

## Enabling it

1. `components: { cnpgBackup: { enabled: true } }` in `environments/<deployment>.yaml`,
   then `llz render <deployment>` and commit. That writes `apl-overlay/platformBackups.yaml`
   (and `buckets.cnpg` in `obj.yaml`, which every deployment carries).
2. Apply the `object-storage` root if this deployment predates the cnpg bucket. The
   platform key gains the bucket on its own: the in-cluster rotator sees the seeded key
   does not grant it and rotates it once (no operator step).
3. The apl-overlay reconciler writes `env/settings/platformBackups.yaml` on `apl-<env>`
   (a key-level merge — apl-core's other settings in it stand) and apl-operator syncs the
   two otomi-db releases. Each Cluster rolls its instances to add the plugin sidecar.
4. Re-run `bootstrap-openbao` (or wait for the next one). After converge it runs
   `llz ci assert-cnpg-backup --region <deployment>`: the first backup of each database,
   read back from the bucket.
5. Verify by hand if you like:

   ```bash
   kubectl -n keycloak get clusters.postgresql.cnpg.io keycloak-db \
     -o jsonpath='{.status.conditions[?(@.type=="ContinuousArchiving")]}'
   kubectl -n keycloak get objectstores.barmancloud.cnpg.io keycloak-db -o jsonpath='{.spec.configuration.destinationPath}'
   kubectl -n keycloak get backups.postgresql.cnpg.io
   ```

The spec now owns `enabled`, `schedule`, `retentionPolicy` and `pathSuffix` for those
databases: a change made in the App Platform Console's backup settings is reasserted
on the reconciler's next pass. Change the knobs instead.

## What the bootstrap gate proves

`llz ci assert-cnpg-backup` (every bootstrap of a deployment with the component on), for
each database:

1. the **live** Cluster carries the barman-cloud plugin as its WAL archiver (the in-tree
   `barmanObjectStore` path is refused — the alerts read the plugin's metrics);
2. the ObjectStore it names writes to **this deployment's** cnpg bucket and endpoint
   (and, with objProxy, trusts the proxy CA);
3. continuous WAL archiving works;
4. an on-demand `Backup` completes;
5. through a temporary read-only key, the base backup and its end WAL segment are in the
   bucket **under the exact path a recovery reads** — derived from the live ObjectStore
   and serverName, not from the render — and are ciphertext where objProxy is on;
6. Prometheus has loaded the `llz-cnpg-backup` rules.

When it fails, its message names the next step. The common ones:

| Message says | Do |
|---|---|
| `was never wired for backups` | `env/settings/platformBackups.yaml` on `apl-<env>` (and `llz_apl_overlay_platform_backups_target_unusable`), `env/settings/obj.yaml`'s `buckets.cnpg`, then apl-operator's sync |
| `ALREADY HOLDS an archive` | the destination has a previous cluster's archive — restore from it, or bump `backupGeneration` |
| `Restart them` | the ObjectStore gained `endpointCA` after the instances started: `kubectl cnpg restart <cluster> -n <ns>` |
| `IN-TREE barmanObjectStore` | `apps.cnpg.useBackupPlugin` was set false; set it back |

## Restoring a database

A CNPG Cluster is bootstrapped once. Restoring means **recreating the Cluster with a
`bootstrap.recovery`** from the object store — apl-core's `databases.<db>.recovery` and
`externalClusters` (its own fixture shows the shape). PVC snapshots and clones are not a
restore path here: the platform denies snapshot- and clone-sourced PVCs at admission.

> **Lab-gated.** The order below follows apl-core's templates and CNPG's recovery
> semantics; it has not been run end to end on a managed cluster by this repo. The e2e
> drill proves the archive restores into a fresh cluster, not this in-place procedure.
> Rehearse it on a non-production deployment first.

Keycloak shown; for Harbor use `harbor`, `harbor-otomi-db`, database `registry`, owner
`harbor`, namespace `harbor`.

1. **Pick the backup.** `kubectl -n keycloak get backups.postgresql.cnpg.io` (or list
   `keycloak-<N>/keycloak-db/base/` in the bucket). Note the current generation `N`
   (`pathSuffix` in `env/settings/platformBackups.yaml`).
2. **Point a second ObjectStore at generation N** — the restore source. apl-core's own
   `keycloak-db` ObjectStore is about to move to `N+1`:

   ```yaml
   apiVersion: barmancloud.cnpg.io/v1
   kind: ObjectStore
   metadata: { name: keycloak-db-restore-source, namespace: keycloak }
   spec:
     configuration:
       destinationPath: s3://<prefix>-cnpg-<env>/keycloak-<N>
       endpointURL: https://<cluster>.linodeobjects.com
       s3Credentials:
         accessKeyId:     { name: linode-creds, key: S3_STORAGE_ACCOUNT }
         secretAccessKey: { name: linode-creds, key: S3_STORAGE_KEY }
       wal: { compression: gzip }
       data: { compression: gzip }
   ```

   (With objProxy on, the Kyverno policy adds `endpointCA` on create.)
3. **Bump the generation:** `components.cnpgBackup.backupGeneration: <N+1>`, `llz render`,
   commit. The rebuilt cluster then archives into `keycloak-<N+1>` — CNPG refuses to
   archive over an existing archive, and that refusal is also what keeps generation N,
   the one being restored from, untouched.
4. **Set the recovery** in `env/databases/keycloak.yaml` (the `AplDatabase` CR) on the
   `apl-<env>` branch:

   ```yaml
   spec:
     recovery:
       source: keycloak-backup
       database: keycloak
       owner: keycloak
       # optional point-in-time: recoveryTarget: { targetTime: "2026-10-15 03:00:00+00" }
     externalClusters:
       - name: keycloak-backup
         plugin:
           name: barman-cloud.cloudnative-pg.io
           parameters: { barmanObjectName: keycloak-db-restore-source, serverName: keycloak-db }
   ```

   Wait for apl-operator to sync the release, so the Cluster spec carries the recovery
   bootstrap (CNPG ignores `bootstrap` on a cluster that already exists).
5. **Recreate the Cluster:** `kubectl -n keycloak delete clusters.postgresql.cnpg.io keycloak-db`.
   apl-core re-renders it and CNPG runs a full recovery. Keycloak is down until it is
   Ready; `kubectl -n keycloak get clusters.postgresql.cnpg.io keycloak-db -w`.
6. **After it is healthy:** restart Keycloak (or Harbor core), take an on-demand backup
   into the new generation, then set `recovery: {}` and `externalClusters: []` back and
   delete `keycloak-db-restore-source`. Generation N stays in the bucket until you delete
   it; it is no longer under any retention policy.

## Restore after the cluster was lost

1. **Rebuild** with Terraform. Do not destroy `object-storage` — the cnpg bucket is what
   you restore from (the destroy refuses while it holds anything, unless
   `drain_data_buckets=true`).
2. **Restore OpenBao first** ([openbao-backup-restore](openbao-backup-restore.md)). With
   objProxy on, the backups are SSE-C under `secret/obj/ssec`; without it you still need
   the platform key re-seeded for apl-core.
3. Let the platform converge with `cnpgBackup` **and the `backupGeneration` bump of
   step 3 above already committed**, then follow [Restoring a database](#restoring-a-database)
   from step 2 for each database. Without the bump the new clusters fail WAL archiving
   with "Expected empty archive" — loudly (`LLZCnpgWalArchivingFailing`, and the gate) —
   which is the archive protecting itself.

## Restore drill

On the release-e2e deployment the gate runs with `--drill` (the `restore_drill` input):
before the backup it creates a canary database `llz_canary_<nonce>` in the **live**
cluster (a CNPG `Database` CR), then bootstraps a one-instance scratch Cluster in
`llz-cnpg-drill-<db>` by barman recovery to exactly that backup, reading the archive
through a temporary **read-only** key with no WAL archiver of its own, and fails unless a
`psql` Job there finds the canary. The canary is dropped and the scratch namespace
removed afterwards. Refused on any deployment that is not an e2e lane.

Do the same by hand each quarter on a non-production deployment, timing it: the time
from `delete clusters.postgresql.cnpg.io` to Ready in [Restoring a database](#restoring-a-database)
is that database's RTO.

## RTO and RPO

| Scenario | RTO | RPO |
|---|---|---|
| Bad data written (a dropped realm, a deleted project) | the in-place restore: tens of minutes for these sizes, plus apl-operator's sync | point-in-time to just before it, within `retention` — WAL is archived continuously |
| Cluster lost, region intact | hours: rebuild, OpenBao restore, converge, then each database | the last archived WAL segment: CNPG's 5-minute `archive_timeout` bounds it while archiving works |
| Region lost | rebuild elsewhere, same order | as above **if** the bucket's region survived; the bucket is in the deployment's region |

`LLZCnpgWalArchivingFailing` is what tells you the RPO stopped advancing.

## The alerts

On the barman-cloud plugin's metrics for the `harbor` and `keycloak` namespaces
([prometheusrule.yaml](https://github.com/akamai-consulting/lke-landing-zone/blob/main/platform-apl/components/cnpgBackup/cnpg-backup/prometheusrule.yaml)).
apl-core's own `BackupHarborDbFailing` / `BackupKeycloakDbFailing` read the deprecated
in-core `cnpg_collector_*` gauges, which the plugin supersedes.

| Alert | Fires when | First check |
|---|---|---|
| `LLZCnpgBackupFailing` (warning) | the newest backup attempt failed | `kubectl -n <ns> get backups.postgresql.cnpg.io --sort-by=.metadata.creationTimestamp`; the instance's `plugin-barman-cloud` container log |
| `LLZCnpgBackupStale` (critical) | no successful base backup for 49h | the ScheduledBackup; the archiver |
| `LLZCnpgBackupNeverSucceeded` (warning) | backups configured 26h, none succeeded | the Cluster's `ContinuousArchiving` condition — "Expected empty archive" means bump `backupGeneration` |
| `LLZCnpgWalArchivingFailing` (warning) | an instance's archiver failed more recently than it succeeded | the `plugin-barman-cloud` log: credentials, endpoint, CA |
| `LLZCnpgBackupMetricsAbsent` (warning) | no plugin backup series for either database | is the plugin wired (the gate), are the instances scraped |

## Disabling it, and destroying the bucket

- **Disabling the component stops LLZ asserting the backups; it does not turn them off.**
  `llz render` only adds files, so also delete `apl-values/<env>/apl-overlay/platformBackups.yaml`,
  then set `enabled: false` for each database in the Console's backup settings (or in
  `env/settings/platformBackups.yaml`). The bucket's contents stay.
- **Destroying `object-storage`** refuses while the cnpg bucket holds any version, unless
  the dispatch sets `drain_data_buckets=true` (`llz ci openbao-backup-destroy-guard` guards
  it with the OpenBao and Velero buckets).
- **A cluster destroy** never touches the bucket, except on the e2e lane's own
  deployments with `drain_data_buckets=true`, where `llz ci drain-obj-buckets` empties it
  so the next run's fresh clusters can archive.
