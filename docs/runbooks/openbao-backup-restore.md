# OpenBao backup and restore

**Applies to:** a deployment whose OpenBao data is lost or damaged and must come back
from a backup; or `LLZOpenBaoBackupFailing` / `LLZOpenBaoBackupStale` /
`LLZOpenBaoBackupNeverSucceeded` is firing; or you are deciding who may hold the backup
bucket, or what else may back the cluster up. Not for a sealed or
half-bootstrapped OpenBao whose data is intact — that is
[bootstrap-openbao](bootstrap-openbao.md).

> **Never restore one half of an HA pair from the other half's snapshot.** The two
> clusters are independent stores with their own keyrings and their own static seal
> keys; a snapshot of one is not a backup of the other. `llz openbao restore` refuses a
> snapshot whose cluster id does not match the live cluster unless you pass `--force` —
> do not pass it to get past that check. Pass it only when you have **rebuilt** this
> deployment's OpenBao and know the snapshot is this deployment's own.
>
> **Never lose `OPENBAO_SEAL_KEY`.** A snapshot is ciphertext that only the seal key
> of the cluster that took it can open. Without that key every backup is unrecoverable.
>
> **Rebuilding after a loss? Turn the backup off first.** A rebuilt OpenBao is a new,
> empty cluster that starts backing itself up into the same bucket. Set
> `components.openbaoBackup.enabled: false` for the deployment *before* the rebuild's
> bootstrap, and back on after the restore — see
> [the rebuild procedure](#restore-the-cluster-was-lost-and-rebuilt).

<!-- toc -->
## Contents

- [What is backed up, when, and where](#what-is-backed-up-when-and-where)
- [What the bucket gives away](#what-the-bucket-gives-away)
- [Other backups of the cluster must exclude `llz-openbao`](#other-backups-of-the-cluster-must-exclude-llz-openbao)
- [What a restore needs](#what-a-restore-needs)
- [List the backups](#list-the-backups)
- [Restore: the data is damaged, the cluster is fine](#restore-the-data-is-damaged-the-cluster-is-fine)
- [Restore: the cluster was lost and rebuilt](#restore-the-cluster-was-lost-and-rebuilt)
- [Restore: an HA pair](#restore-an-ha-pair)
- [After a restore](#after-a-restore)
- [The alerts](#the-alerts)
- [Upgrading a deployment that predates backups](#upgrading-a-deployment-that-predates-backups)
- [What proves it works](#what-proves-it-works)

<!-- /toc -->

## What is backed up, when, and where

The `openbaoBackup` component (on by default) runs the `openbao-backup` CronJob in its
own `llz-openbao-backup` namespace. Every six hours by default (`23 */6 * * *`;
`components.openbaoBackup.backupIntervalHours`, any divisor of 24) it:

1. logs in to OpenBao with the `openbao-backup` Kubernetes-auth role — a client
   certificate at the transport, the pod's ServiceAccount token at the auth mount, the
   same way the other in-cluster jobs authenticate (see [docs/secrets.md](../secrets.md));
2. reads the backup bucket's key from `secret/obj/openbao-backup`;
3. takes a Raft snapshot (`GET /v1/sys/storage/raft/snapshot`) from the **active** node;
4. verifies the archive (gzip, tar, `SHA256SUMS` over `meta.json` and `state.bin`) —
   a body that is not a snapshot fails the run instead of becoming the newest backup;
5. uploads it to `<prefix>-openbao-backup-<deployment>` as
   `openbao-raft/<UTC timestamp>_<cluster id>.snap`, and reads it back to prove the
   bucket returns the same bytes;
6. deletes all but the newest **28** (seven days at the default interval;
   `components.openbaoBackup.backupKeep`) **of each OpenBao cluster id** in the bucket.
   It never deletes the snapshot it just wrote, never acts on a listing that does not
   contain that snapshot, and never touches an object whose name it did not produce.

**Retention is per cluster id, and that is what keeps a rebuild from erasing the
history.** A rebuilt OpenBao is a new Raft cluster with a new id, backing up an empty
store. If retention counted the whole bucket, its empty snapshots would push every
snapshot of the lost data out within a week. Counted per id, nothing the rebuilt cluster
writes can delete the old cluster's newest 28 — only that cluster could, and it is gone.
The cost: a bucket keeps up to 28 snapshots of every cluster id it has ever seen. Delete
a dead cluster's snapshots by hand once you no longer need them; the job never will.

The bucket is **versioned**: a snapshot retention deletes (or anything else deletes or
overwrites) stays recoverable as a noncurrent version for 30 days
(`openbao_backup_noncurrent_days` on the `llz-object-storage` module) before a lifecycle
rule expires it. That is a net under retention and under mistakes — not protection from
someone holding a write key, who can delete versions too.

**Bootstrap takes the first backup.** Every `bootstrap-openbao` run, after converge, runs
a Job from this CronJob and verifies the snapshot it uploaded through the restore path
(`llz ci assert-openbao-restore --backup-only`). A producer that cannot work on this
deployment fails that bootstrap, with the Job's log, instead of the first scheduled tick.

The `openbao-backup` policy is `read` on `sys/storage/raft/snapshot` plus a read of its
own bucket key. It cannot restore (that is `update` on the same path), and it cannot
read any secret's plaintext.

The bucket key is a separate Linode Object Storage key scoped to the backup bucket
alone, minted at bootstrap and rotated by the reconciler with the platform key. It is
deliberately **not** the platform key: that one is handed to apl-core's Loki and Harbor,
and anything holding it could otherwise delete every backup.

Each deployment backs up its own OpenBao into its own bucket. In an HA pair both
halves back up independently.

**The backups live in the same region as the cluster** (the deployment's
`cluster.objectStorage.cluster`), so a regional Object Storage outage takes the backups
with the cluster. A separate backup region is not configurable yet — it touches the
bucket placement, the key scoping, the in-cluster endpoint, the CronJob's egress and the
restore path at once — and is tracked as a follow-up. Until then, an HA pair is the
cross-region copy: each half is an independent store in its own region.

**The bucket outlives the deployment.** Destroying the cluster never touches it.
Destroying object-storage (`terraform.yml`, module `object-storage`) refuses — before
anything is drained or deleted — while the bucket holds any snapshot, unless you pass
`drain_data_buckets=true`; copy the snapshots out first if you might want them.

## What the bucket gives away

Linode Object Storage offers no managed keys (no SSE-KMS, and `PutBucketEncryption`
is not implemented), so the bucket is not encrypted at rest by the provider. The
snapshot does not need it to be: OpenBao encrypts every stored **value** under its
barrier keyring before it reaches Raft, and the keyring is wrapped by the root key,
which the static seal key wraps in turn.

Someone who can **read** the bucket gets:

| They can see | They cannot see |
|---|---|
| Storage **key names** in the Raft FSM: KV secret paths (`secret/<team>/<name>`), policy and auth-role names, lease and token-accessor paths | Any secret value, token, policy body, auth configuration, or the barrier keyring |
| Raft membership in `meta.json` (node ids and addresses) | The root key or the recovery-key configuration |
| How many snapshots exist, their sizes and timestamps (so: roughly how much the store changes) | Anything else, without the seal key |

**Bucket read plus `OPENBAO_SEAL_KEY` is everything in OpenBao.** That is the
confidentiality boundary — and today both halves sit in the **same place**. The
`infra-<deployment>` GitHub environment holds `OPENBAO_SEAL_KEY` **and**
`LINODE_API_TOKEN`, and that token can mint a read key on the backup bucket (it is how
`llz openbao restore` reads it). So the real boundary is **who can run a workflow in the
`infra-<deployment>` environment** (or change one that does): anyone who can is one
`llz` command from the whole store. Treat that environment's deployment protection rules
and required reviewers as the control on OpenBao's backups, not the bucket's privacy.

What to do about it:

- **Escrow the seal key offline** (a password manager or HSM-backed vault outside
  GitHub), so losing GitHub is not losing every backup. The environment secret is
  write-only, so do this when you first create it — it cannot be read back later.
- **Restrict who can run workflows in `infra-<deployment>`** with environment protection
  rules (required reviewers, deployment branches).
- The planned structural fix is to take the seal key out of the environment that holds the
  Linode token altogether, with custody of the seal and recovery keys outside GitHub —
  the custody ADR proposed in [#568](https://github.com/akamai-consulting/lke-landing-zone/pull/568).
  Until that lands, the two live together and the bullets above are the mitigation.

The seal key is never in the bucket and never in an in-cluster identity other than
OpenBao's own pods — which is why the next section matters.

Someone who can **write or delete** the bucket (the backup key, or `LINODE_API_TOKEN`)
can destroy backups — an availability risk, not a confidentiality one. They cannot
plant a usable snapshot: a non-forced restore checks the archive's sealed hashes against
the live keyring and refuses a foreign one, which is one more reason `--force` is only
for a rebuild.

**Why not the objProxy SSE-C gateway.** It would encrypt each snapshot under a key that
lives in OpenBao, so losing OpenBao would lose the means of reading its own backups. The
backup CronJob addresses the bucket virtual-hosted (`<bucket>.<endpoint>`) precisely so
the gateway's CoreDNS rewrite, which matches the bare endpoint host, never captures it.

## Other backups of the cluster must exclude `llz-openbao`

Everything above assumes the seal key never lands in a bucket. A **cluster** backup tool
breaks that the moment it includes the `llz-openbao` namespace: the `openbao-unseal-key`
Secret there **is** `OPENBAO_SEAL_KEY`, and the Raft volumes are a crash-consistent copy
of the store. A Velero backup of every namespace therefore puts seal key and data in one
bucket — whoever can read it has the whole store — and the volume snapshots are a second
restore path that nothing has proven restores.

So: **any Velero `Schedule` / `Backup` (or any other namespace- or volume-level backup)
must exclude `llz-openbao`** — e.g. `excludedNamespaces: [llz-openbao]` beside
`includedNamespaces: ["*"]`. OpenBao is backed up by this component, through the API,
as an archive whose integrity is checked before it is uploaded and again before it is
restored. The landing zone's own `velero` component excludes it from every Schedule,
with a test over every rendered shape and a check of the live Schedule on every
bootstrap ([velero-dr](velero-dr.md)); a Schedule you add yourself needs the same line.

## What a restore needs

| You need | From |
|---|---|
| The snapshot | the backup bucket — read with a temporary key minted from `LINODE_API_TOKEN` (OpenBao holds the backup key, and OpenBao is what you are restoring) |
| The **same** `OPENBAO_SEAL_KEY` the snapshot's cluster used | already in the `openbao-unseal-key` Secret on a cluster that was not rebuilt; restored from the `infra-<deployment>` environment secret by `llz ci bao-seed-seal-key` on one that was |
| A root token **on the cluster you restore into**, exported in your shell | `llz openbao regen-root <deployment>` with 3 recovery shares you hold. (`breakglass-openbao.yml` reconstitutes root from the shares in `infra-<deployment>`, but writes the token back into that environment, where only workflows can read it — `llz openbao restore` runs from your workstation.) |
| kubectl reach to that cluster | your kube-context. The command checks it is **this** deployment's LKE cluster — the cluster id on its nodes must be the one the Linode account labels with the spec's `cluster.clusterLabel` for `--region` — on the dry run and again just before applying, and refuses otherwise. `--force` does not waive this |

After a restore, the store's tokens, leases and **recovery-key configuration** are the
snapshot's. The root token you used to restore is gone.

**Gap, stated rather than hidden:** no workflow wraps `llz openbao restore` yet. A
deployment bootstrapped without `openbao_escrow_pubkey_b64` has its recovery shares only
in GitHub, where a workstation cannot read them, so it cannot produce the root token this
command needs without first running break-glass and recovering the token by other means.
Bootstrap with an escrow key (see [docs/secrets.md](../secrets.md#initial-cluster-bring-up)).

## List the backups

From the instance repo root, with `LINODE_API_TOKEN` exported:

```bash
llz openbao backups --region <deployment>
```

Newest first, with each one's age and the OpenBao cluster id it came from. An empty
list means the CronJob has never uploaded one — see [the alerts](#the-alerts). When the
bucket holds more than one cluster id (what a rebuild leaves behind) it says so: the
newest are then usually the rebuilt cluster's backups of its own empty store.

## Restore: the data is damaged, the cluster is fine

Someone deleted a path, overwrote a policy, or a bad automation run wrote garbage.

1. Pick the snapshot from **before** the damage — not reflexively the newest:

   ```bash
   llz openbao backups --region <deployment>
   ```

2. Dry-run it. Nothing changes; it fetches and verifies the snapshot, finds the active
   pod, and tells you which cluster it would overwrite and whether the ids match:

   ```bash
   llz openbao restore --region <deployment> --snapshot <key>
   ```

3. Get a root token on this cluster and restore:

   ```bash
   llz openbao regen-root <deployment>   # see playbooks/openbao-accounts.md
   export OPENBAO_ROOT_TOKEN=...
   llz openbao restore --region <deployment> --snapshot <key> --yes --confirm restore:<deployment>
   ```

   `--yes` executes; `--confirm` must name the deployment exactly. The command waits for
   OpenBao to come back unsealed and serving, then prints what to do next.

4. Work through [After a restore](#after-a-restore).

## Restore: the cluster was lost and rebuilt

The LKE cluster (or the `llz-openbao` namespace and its volumes) is gone and you have
rebuilt it with the normal pipeline.

0. **Before the rebuild's bootstrap, turn the backup off for this deployment:** set
   `components.openbaoBackup.enabled: false` in `environments/<deployment>.yaml`, run
   `llz render <deployment>`, and commit. Otherwise the rebuilt — empty — OpenBao starts
   backing itself up into the same bucket. Retention can no longer delete the lost
   cluster's snapshots because of it, and `--latest` refuses to guess between the two,
   but there is no reason to fill the bucket with backups of nothing. (If you already
   rebuilt with it on, carry on — nothing pre-loss has been pruned.)
1. **Confirm you still hold the snapshot-era recovery shares** —
   the escrowed ciphertext from the original bootstrap, or the `OPENBAO_RECOVERY_KEY_*`
   you recorded. The rebuild's `bao-init` mints NEW shares and overwrites the copies in
   `infra-<deployment>`; after the restore the store answers only to the OLD ones.
2. Let the rebuild finish. `llz ci bao-seed-seal-key` restores the same static seal key
   from `OPENBAO_SEAL_KEY`, so the new pods can unseal the old data.
3. Regenerate root on the rebuilt (empty) store. The NEW shares are what open it now —
   the rebuild delivered them to you (escrowed) or wrote them to `infra-<deployment>`:

   ```bash
   llz openbao regen-root <deployment>
   export OPENBAO_ROOT_TOKEN=...
   ```

4. Restore. List the backups and note the **lost** cluster's id — the one whose snapshots
   predate the loss. If the backup ran on the rebuilt cluster, the bucket now holds two
   ids and `--latest` on its own refuses; name the old one with `--from-cluster`. The ids
   will NOT match the live (new) cluster, so the dry run says `MISMATCH` and the restore
   needs `--force`. The dry run also proves the kube-context is this deployment's LKE
   cluster, whatever `--force` says:

   ```bash
   llz openbao backups --region <deployment>
   llz openbao restore --region <deployment> --latest --from-cluster <lost-cluster-id>
   llz openbao restore --region <deployment> --latest --from-cluster <lost-cluster-id> --force --yes --confirm restore:<deployment>
   ```

5. Put the snapshot-era recovery shares back into `infra-<deployment>`
   (`OPENBAO_RECOVERY_KEY_1`..`3`) — the ones the rebuild wrote no longer open this store,
   and the next break-glass would fail on them.
6. Turn the backup back on (`enabled: true`, or delete the line), re-render, commit.
7. Re-run `bootstrap-openbao.yml` for the deployment. Its configure step re-applies the
   platform policies, roles and auth-mount configuration against the restored store —
   a rebuilt cluster usually has a new domain, which the team-login (Keycloak) mount
   points at — and it takes and verifies a first backup of the restored store.
8. Work through [After a restore](#after-a-restore).

## Restore: an HA pair

The two clusters of a pair are independent stores; `llz openbao set` keeps the
operator-managed paths in step by writing both. So:

- **Restore only the damaged deployment, from its own bucket.** `--region` selects the
  bucket; your kube-context selects the cluster; the command refuses unless the context's
  LKE cluster is `--region`'s own (the spec's `cluster.clusterLabel`), so a context
  pointing at the peer is caught even with `--force`.
- **The peer is the more recent copy of every dual-written path.** After the restore,
  run the drift check in [docs/secrets.md](../secrets.md#drift-verification) against both
  halves and re-apply the newer values with `llz openbao set` (which dual-writes).
- Paths sourced or rotated **in-cluster** differ between the halves by design
  (`secret/obj/*`, `secret/grafana/admin`, `secret/otel/ingress`, …); do not copy them
  across. [After a restore](#after-a-restore) covers them.
- If **both** halves are lost, restore each from its own bucket, one at a time.

## After a restore

The store is now exactly as it was when the snapshot was taken.

1. **Tokens.** Every token issued after the snapshot is invalid, including the root token
   you restored with. In-cluster workloads re-authenticate on their own on their next
   cycle; ESO rebuilds its client within its store-requeue interval.
2. **Rotated credentials may name keys that no longer exist.** The rotators revoke
   superseded keys, so a snapshot older than a rotation can hold a credential Linode
   has since deleted:
   - `secret/obj/platform`, `secret/obj/openbao-backup` — the reconciler's `linode-creds`
     lane re-mints an object-storage key that no longer exists on the account on its next
     pass, without waiting for its age threshold. Watch Loki and Harbor writes recover.
   - `secret/linode/api-token` — the in-cluster PAT; its superseded copies are drained
     after a seven-day grace. Re-mint it now:
     `gh workflow run secret-rotation.yml -f scope=linode-pat-propagate-only -f confirm=rotate:linode-pat-propagate-only -f reason="post-restore re-mint"`.
   - `secret/linode/broad-pat` (only where `broadPatRotator` is enabled) — if the
     snapshot predates the rotator's last run by more than its grace window, re-seed it
     the way bootstrap does (re-run `bootstrap-openbao.yml`).
3. **Operator writes since the snapshot** are gone. Re-apply them with `llz openbao set`.
4. **Database admin credentials** (`secret/infra/db-admin/*`): if `rotate-db-admin` ran
   after the snapshot, the restored password is the old, dead one. Rotate again
   (`secret-rotation.yml` scope `db-admin`) so OpenBao and the database agree.
5. Confirm the next scheduled backup succeeds — or run one now:

   ```bash
   kubectl -n llz-openbao-backup create job --from=cronjob/openbao-backup openbao-backup-manual
   kubectl -n llz-openbao-backup logs -f job/openbao-backup-manual
   ```

## The alerts

| Alert | Severity | Means | First check |
|---|---|---|---|
| `LLZOpenBaoBackupFailing` | warning | the newest backup Job failed and nothing has succeeded since — fires ~15m after the first failure, including the very first run | `kubectl -n llz-openbao-backup get jobs` and the failed Job's logs |
| `LLZOpenBaoBackupStale` | **critical** | no successful backup for over two intervals plus an hour (13h at the default 6h) — derived from the CronJob's own schedule, so it follows `backupIntervalHours` | as above; every hour it holds is an hour of writes with no restore point |
| `LLZOpenBaoBackupNeverSucceeded` | warning | the CronJob's first tick is over an hour past and it has never succeeded **or** failed — it is not being scheduled at all (suspended, or a missed deadline) | `kubectl -n llz-openbao-backup get cronjob openbao-backup -o yaml` |

The Job's last log line is a JSON record (`"event":"openbao-backup"`). On failure the
error names the step: login (role missing → re-run `bootstrap-openbao.yml`), key read
(`secret/obj/openbao-backup` absent → apply the object-storage root and let the
reconciler mint it), snapshot (`403` → policy missing, same remedy), upload or
read-back (bucket or key wrong — the S3 error code is in the message).

## Upgrading a deployment that predates backups

The CronJob arrives with the template upgrade, but three things it depends on are not
in the cluster yet. Until all three are done every tick fails, converge reports the
failed Job, and `LLZOpenBaoBackupFailing` fires — loudly, on purpose. Step 2's bootstrap
also takes a first backup and fails if it cannot, so do the steps in this order.

1. **Apply the object-storage root** (`terraform.yml`, module `object-storage`). It
   creates `<prefix>-openbao-backup-<deployment>`.
2. **Re-run `bootstrap-openbao.yml`** for the deployment. Its configure step writes the
   `openbao-backup` policy and role and the updated `linode-rotator` / `reconciler-read`
   policies; its mint step seeds `secret/obj/openbao-backup`. (Without this step the
   reconciler would also try to mint the key and be refused the write.)
3. **Let Argo sync `llz-openbao-platform` 0.1.34**, which admits the backup pod to
   OpenBao's `:8200`. It is pinned by the template, so the upgrade carries it.

To opt out instead, set `components.openbaoBackup.enabled: false` for the deployment and
re-render. Nothing else backs OpenBao up.

## What proves it works

`llz ci assert-openbao-restore` runs in every release e2e (its own `restore_drill`
input), in the bootstrap job just before the root token is revoked. It writes a canary,
runs a Job built from the real CronJob, fetches the exact snapshot that Job uploaded,
overwrites the canary, restores the snapshot through the same code `llz openbao restore`
uses, fails unless the canary reads back its snapshotted value, and re-writes any
rotated credential the rollback reverted. It restores in place, so it refuses any
deployment that is not an e2e lane's.

Every other deployment runs the safe half on each bootstrap
(`assert-openbao-restore --backup-only`): the real Job, then a fetch and verify of
exactly what it uploaded, through the restore path, with nothing restored. See
[docs/workflows/llz-bootstrap-openbao.md](../workflows/llz-bootstrap-openbao.md).
