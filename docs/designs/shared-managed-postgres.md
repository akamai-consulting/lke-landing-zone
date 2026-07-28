# Design: VPC-attached Managed Postgres (`llz-databases`)

> Status: in progress. Terraform module + embedded root + `llz render` wiring
> landed; the OpenBao seed command and CI workflow jobs are the remaining wiring
> (see "Remaining work"). Motivated by the gsap-apl managed-app-platform buildout,
> which needed a shared Postgres but had **no** IaC for one (the cluster was
> provisioned ad-hoc, public, non-VPC).

## What it provides

**Zero, one, or several** Linode Managed PostgreSQL clusters per deployment, each
**inside the cluster VPC** (no public endpoint). Downstream application platforms
(e.g. a Crossplane `provider-sql` layer) carve per-app logical databases + roles
out of a cluster, reaching it over the private network. Each cluster's admin
credentials are seeded into OpenBao at `secret/platform/db-admin/<name>` so ESO
can publish them to the consumers.

Opt-in, and the opt-in needs no flag: an instance that declares no
`spec.cluster.databases` entries renders `databases = {}`, and the root applies
cleanly while provisioning nothing.

### Why 0-n and not one shared cluster

One shared cluster is the expected shape and the recommended default — logical
databases inside it are nearly free, and a second cluster is a second bill, a
second maintenance window and a second admin credential to rotate. But "exactly
one" is the wrong thing to hard-code, because the cases that break it are real:

- an app pinned to a **different major version** than the shared cluster,
- a workload whose IOPS or memory profile warrants its **own node type**, so a
  noisy tenant cannot starve the rest,
- a **compliance boundary** that must not share a Postgres instance,
- and the **migration window itself** — cutting from an old cluster to a new one
  (see below) means both exist at once, which "one per deployment" cannot express.

So the spec is a map and the root is a `for_each`. A deployment that wants one
cluster writes one entry; the cost of the general case is a single map level.

## Shape (mirrors `llz-object-storage`)

```
spec.cluster.databases           →  llz render  →  databases/<env>.tfvars
  <name>:                                             │   databases = { <name> = { … } }
    region, vpcId, subnetId,                          ▼
    engineVersion, type,            databases root: for_each = var.databases
    clusterSize                                       │
                                                      ▼  (one module call per cluster)
                                    terraform-modules/llz-databases
                                      (linode_database_postgresql_v2,
                                       private_network = { vpc_id, subnet_id,
                                                           public_access = false })
                                                      │  outputs, keyed by <name>
                                                      ▼
                                    llz ci seed-db-admin
                                      → secret/platform/db-admin/<name>
```

Spec example — one shared cluster plus a separately-sized one:

```yaml
spec:
  environments:
    primary:
      cluster:
        databases:
          shared:
            region: us-ord      # MUST match the VPC's region
            vpcId: 575244       # typically the cluster's own VPC
            subnetId: 12345
            engineVersion: "16"
            type: g6-dedicated-2
            clusterSize: 2      # HA
          analytics:
            region: us-ord
            vpcId: 575244
            subnetId: 12345
            type: g6-dedicated-8
            clusterSize: 1
```

`region_suffix` is always the env name (like object storage). `vpc_id`/`subnet_id`/
`cluster_size` render as HCL numbers.

### Per-environment only — `spec.defaults` is refused

Unlike `objectStorage`, `databases` is **not** inherited from
`spec.defaults.cluster`, and `llz validate` rejects the block there rather than
ignoring it.

The fields that identify a cluster are `vpcId`/`subnetId`, and those are
per-environment by construction: each env normally has its own VPC, and a VPC
cannot span regions. An inherited `vpcId` would attach one env's database to
another env's network, or fail at apply against a VPC in the wrong region. There
is no instance-wide default worth having, so the honest answer is to refuse
rather than to guess.

`Defaults` embeds a `Cluster`, so the field stays *syntactically* settable under
`spec.defaults` — which is exactly why the check exists. Without it the block is
accepted, silently dropped by `mergeCluster`, and no database is ever
provisioned. The validator and the omission in `mergeCluster` are a pair; neither
should be changed alone.

The exception — two envs deliberately sharing one VPC via `spec.networks` — is
the case where writing the block per env costs three lines and makes the sharing
visible at the point it happens.

### The key is identity, in three places at once

`databases` is a **map**, not a list, and the key is load-bearing. `<name>` is
simultaneously:

1. the middle segment of the Linode label — `platform-<name>-<env>`,
2. the Terraform state address — `module.databases["<name>"]`, and
3. the OpenBao path — `secret/platform/db-admin/<name>`.

That is what makes adding or removing a cluster safe: the survivors keep all three.
Under a list, identity would be the position, so deleting the first of three
entries would re-plan the other two onto each other's state — a destroy/recreate of
two production databases from a one-line spec edit.

It also means the key is not free-form prose. `llz validate` enforces the
lowercase-alphanumeric-dash shape at spec-edit time, and the module re-checks it,
because the Linode API rejects a bad label at **apply** — after the plan looked
clean and after any sibling clusters in the same apply were already created.

### Why `_v2` / VPC

`linode_database_postgresql_v2` (plugin-framework) is the only resource with the
`private_network` attachment. `public_access=false` is the whole point — a
Managed DB otherwise ships a public `g2a.akamaidb.net` endpoint (TLS-only, but
internet-reachable). VPC-only matches the platform's security posture.

### `tofu validate` does not check nested attribute values

`private_network`/`updates` are nested **attributes**, and `validate` type-checks
the assignment but not the nested values against the provider schema. `plan` does.
The module shipped `updates.day_of_week = "sunday"` where the provider wants the
number `7` (1 = Monday … 7 = Sunday): `make tf-validate` and `tf-validate-roots`
both passed, and every `terraform plan` would have failed with *"Inappropriate
value for attribute `updates`: a number is required"*. Neither gate covers this
class — a plan against the released provider is what catches it.

Two things that hid it, both worth remembering:

- The repo's own gates run `validate`, never `plan` (a plan needs credentials).
- A local `~/.terraformrc` **dev override** silently substitutes a working-copy
  build of the linode provider for the released one, so a clean local run proves
  nothing about the version CI resolves. This branch was already bitten once by
  the same override (the `root_username` sensitivity). Verify with
  `TF_CLI_CONFIG_FILE=/dev/null`.

## Consuming it: four things Linode Managed Postgres does differently

This module hands over a cluster; a downstream layer (Crossplane `provider-sql`
in gsap-apl's case) carves per-app databases out of it. Each of the following cost
that build-out a debugging cycle, and none is guessable from the provider docs —
they are properties of the **Aiven-backed** platform Linode runs, not of Postgres.

1. **There is no `postgres` maintenance database.** The bootstrap DB is
   **`defaultdb`**. `provider-sql`'s `ProviderConfig` defaults to connecting to
   `postgres`, so it fails to connect at all until you set
   `defaultDatabase: defaultdb`. The failure surfaces as a connection error against
   a database name you never chose.

2. **The app's role must OWN its database.** On PostgreSQL 15+ the `public` schema
   is no longer writable by non-owners, so a database created by the admin user
   (`akmadmin`) with only a database-level `GRANT ALL` to the app role still fails
   the app's first `CREATE TABLE` with `permission denied for schema public`. Set
   the database's `owner` to the app's own role — it then owns `public` via
   `pg_database_owner`. That is also the right least-privilege line: each app fully
   owns its schema and never touches the admin's.

3. **The CA is base64-encoded.** `ca_cert` (seeded as `ca`) must be decoded before
   it is written to a trust file. Connect with `sslmode=require` at minimum;
   `verify-full` needs the decoded CA.

4. **The admin username is fixed** (`akmadmin`) and the provider marks it
   sensitive — which is why this module's `root_username` output carries
   `sensitive = true`. `terraform output -json` still reads it, so
   `llz ci seed-db-admin` is unaffected. Note this is the *same* username on every
   cluster: with 0-n clusters the username no longer identifies which one you are
   connected to, so the discriminator is the OpenBao path (`db-admin/<name>`) and
   the endpoint, not the credential.

Points 1–3 are consumer-side, so they belong to whatever layer carves the logical
databases — but they are recorded here because this is the doc someone reads
*before* building that layer.

## Migration: existing public cluster → VPC (gsap-apl `gsap-postgres`, id 490457)

The live `gsap-postgres` cluster is **public, non-VPC**, Aiven-platform
(`rdbms-default`), pg 18.4, `us-ord`, HA. It was created out-of-band and is not in
any TF state. Two paths:

**A. Greenfield + data migration (recommended).** Linode does **not** support
attaching an existing Managed DB to a VPC in place, and the VPC path provisions on
a different platform/network. The 0-n shape matters here: the old and new clusters
**coexist** for the length of the migration, as two entries under
`spec.cluster.databases`, and the cutover is the deletion of one entry. Under a
one-cluster-per-deployment model there was nowhere to put the target.

1. Add a second entry — say `shared` — and apply the `databases` root. It
   provisions `platform-shared-<env>` alongside the old cluster and leaves any
   other entry untouched (separate `for_each` keys, separate state addresses).
   **Set its `engineVersion` to at least the source cluster's major version.**
   `gsap-postgres` runs **pg 18.4** and the module defaults to **`"16"`**, so
   taking the default here provisions a target two majors OLDER than the source —
   and `pg_restore` from an 18 dump into a 16 server is not supported. It fails at
   restore time, after the new cluster is already provisioned and paid for.
   Postgres dump/restore is forward-compatible only.
2. `pg_dump` from `gsap-postgres` (public, over TLS) → `pg_restore` into the new
   cluster from a bastion/job **inside the VPC** (the new cluster has no public IP).
   (Downstream logical DBs are Crossplane-provisioned and small; or dump per-app.)
3. Seed `secret/platform/db-admin/shared` with the new cluster's admin creds
   (`llz ci seed-db-admin`), force-sync ESO, let Crossplane re-provision per-app
   DBs/roles against the new endpoint. The old cluster's own
   `db-admin/<name>` stays intact, so a rollback is a consumer-side pointer change
   rather than a re-seed.
4. Cut consumers over (they read the endpoint from `gsap-db-endpoint`, which the
   ESO re-seed updates), verify, then remove the old entry from the spec — the
   apply destroys only that cluster — and delete 490457 if it was never imported.

**B. Import (only if staying public).** `terraform import` 490457 into the
`databases` root as `module.databases["<name>"].linode_database_postgresql_v2.this`
— but this keeps the public endpoint and defeats the feature's purpose; not
recommended. Note the root deliberately does **not** expose `public_access` (nor
`label_prefix`, nor `maintenance`): the module defaults `public_access` to `false`
and the root never overrides it, so VPC-only is not a setting an instance can
drift off. Taking this path means first adding the field to the root's
`map(object({…}))` and threading it through — a visible, reviewable change, which
is the point.

The cutover is a deliberate, scheduled data migration — not a GitOps side effect.
Downstream `Database`/`Role`/`Grant` CRs are `deletionPolicy: Orphan`, so they
survive the endpoint change; only the admin secret + endpoint move.

## Remaining work (this branch)

- `scaffold_spec.go` — emit a commented-out `databases:` block in scaffolded specs
  (commented, because zero clusters is the correct default).
- `docs/landing-zone-spec.md` / `docs/workflows/llz-terraform.md` — document the
  new spec fields + jobs.
- **A `plan` in CI.** `validate` demonstrably does not cover nested attribute
  values (see above), so the root's first real plan is currently an operator's.
  A credential-less plan is not possible against the Linode provider, but the
  e2e lane could plan the `databases` root with an empty `databases = {}` and a
  real token to at least exercise provider schema binding.

Done on this branch: `validate.go` now checks `spec.cluster.databases` (key
format, required region/vpcId/subnetId, region-vs-cluster.region mismatch,
`clusterSize ∈ {1,2,3}`).

### The seed + workflow wiring, as built

`llz ci seed-db-admin` reads the root's single `connections` output and writes
`endpoint`, `port`, `username`, `password`, `ca`, `sslmode=require` to
`secret/platform/db-admin/<name>`, stamped with `rotated_at`. It is a no-op on an
empty map *and* on a state that has no `connections` output at all, so it is safe
on a deployment that predates this root.

Two decisions the open questions above left:

- **It updates a stale path; it does not blind-skip one.** `ci_mint_objkeys.go`
  skips an already-seeded path because re-*minting* would orphan a live Linode
  key. Nothing is minted here — the credential is the provider's — so the same
  rule would strand every consumer on a dead credential after a cluster is
  recreated. The command compares the stored password and rewrites only when it
  differs. An *unreadable* path still fails closed: overwriting a live credential
  we failed to read is indistinguishable from a successful rotation.
- **It never prunes.** Removing a cluster from the spec leaves its OpenBao path.
  At that moment the cluster usually still exists (the spec edit precedes the
  destroy), and the admin credential is the only way back into it for a final
  `pg_dump` — so deleting it automatically would destroy the escape hatch at
  exactly the wrong time. The destroy-plan step says so where the approver reads.

The workflow jobs mirror object-storage with one deliberate difference:
`apply-databases` does **not** ride along on `module: cluster`. Buckets are a
platform dependency every cluster needs; a Managed Postgres is opt-in, billed and
slow to provision, so it is only ever created when it was asked for.

The bootstrap seed is gated on `llz ci db-declared`, which reads the rendered
tfvars — exact, because `DatabasesTFVars` omits the `databases` assignment
entirely when the map is empty. Without the gate, every instance that declares no
databases would still initialize the root on the critical bootstrap path.

## Admin-credential rotation, as built

`llz ci rotate-db-admin` rotates `secret/platform/db-admin/<name>`. It is
due-based (default 80d, under the 90d inventory SLA) and `--apply`-gated.

**Why it is not shaped like the other rotators.** Every other rotator in this
repo is mint → verify → swap → drain: mint a second credential, prove it works,
publish it, then revoke the first. A bad mint can never break a consumer, because
the old credential is live the whole time.

That is unavailable here. Linode fixes the admin user to `akmadmin` and exposes
exactly one mutation — `POST /v4/databases/postgresql/instances/{id}/credentials/reset`
— which regenerates the password in place. There is no second credential, no
overlap window, and no way to choose the new password. The old one dies the
instant the platform applies the reset, before anything has been verified or
persisted.

So the invariant flips from *never break a consumer* (unattainable) to **never
lose the new credential**, and the shape follows from that:

- `--apply` arms the mutation; the default is a report, because the mutation is
  irreversible and unattended.
- The OpenBao write is the only thing between a reset and a locked-out database,
  so a failed write is a loud error carrying the `linode-cli` command to re-read
  the credential — never the credential itself, which would put a live admin
  password in a CI log.
- Rotation is sequential across clusters and stops at the first failure. A loop
  that kept going would turn one lost credential into several.
- The reset is asynchronous, so the command waits for the cluster to return to
  `active` before re-reading. Credentials read mid-`updating` can be the pre-reset
  pair, and storing those would stamp `rotated_at` on a credential that never
  changed — hiding a stale password for another full window. A password that
  comes back **unchanged** is treated as a failed rotation for the same reason.

**The Terraform-state hazard.** `llz ci seed-db-admin` reconciles OpenBao *toward*
state: if the stored password differs from the `connections` output, it overwrites
OpenBao. Correct for its own job (a recreated cluster must not strand consumers on
a dead credential), and actively dangerous after an out-of-band reset — state
holds the OLD password, so a seed run against unrefreshed state would push the
dead credential back over the live one.

The rotator therefore runs `terraform apply -refresh-only` itself after a
successful rotation, and a refresh failure **fails the run** even though the
credential is safely in OpenBao — because the deployment is otherwise left in the
one configuration where a routine seed silently undoes the rotation. The error
says the credential is safe, so nobody "fixes" it by re-running the rotation and
resetting the cluster a second time.

**Scheduling.** Dispatch-only (`scope=db-admin`), and deliberately excluded from
both the monthly cron and `scope=all`: an in-place reset with no overlap window
breaks every live consumer until ESO re-syncs, which is an operator-chosen
maintenance action rather than a cron's decision — and `rotate:all` must never
quietly reset every production database. Age remains continuously visible through
the credential inventory, so nothing goes unnoticed for want of a schedule.

**Not yet proven live.** The rotator is unit-tested end to end against a faked
Linode API (including every post-reset failure mode), but no cluster has been
rotated for real — `gsap-postgres` is the first candidate. Two things to confirm
on that run: that `status` returns to `active` before the new credential is
servable (the wait assumes it does), and that a `-refresh-only` apply actually
picks the new password up into state.
