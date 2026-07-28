# Design: shared VPC-attached Managed Postgres (`llz-databases`)

> Status: in progress. Terraform module + embedded root + `llz render` wiring
> landed; the OpenBao seed command and CI workflow jobs are the remaining wiring
> (see "Remaining work"). Motivated by the gsap-apl managed-app-platform buildout,
> which needed a shared Postgres but had **no** IaC for one (the cluster was
> provisioned ad-hoc, public, non-VPC).

## What it provides

One shared Linode Managed PostgreSQL cluster per deployment, **inside the cluster
VPC** (no public endpoint). Downstream application platforms (e.g. a Crossplane
`provider-sql` layer) carve per-app logical databases + roles out of it, reaching
it over the private network. The cluster's admin credentials are seeded into
OpenBao at `secret/platform/db-admin` so ESO can publish them to the consumers.

Opt-in: an instance that declares no `spec.cluster.databases` config simply never
applies the `databases` root.

## Shape (mirrors `llz-object-storage`)

```
spec.cluster.databases         →  llz render  →  databases/<env>.tfvars
  (region, vpcId, subnetId,                         │
   engineVersion, type,                             ▼
   clusterSize)                    terraform-modules/llz-databases
                                     (linode_database_postgresql_v2,
                                      private_network = { vpc_id, subnet_id,
                                                          public_access = false })
                                                     │  outputs (host/port/user/pass/ca)
                                                     ▼
                                     llz ci seed-db-admin  →  secret/platform/db-admin
```

Spec example:

```yaml
spec:
  environments:
    primary:
      cluster:
        databases:
          region: us-ord      # MUST match the VPC's region
          vpcId: 575244       # typically the cluster's own VPC
          subnetId: 12345
          engineVersion: "16"
          type: g6-dedicated-2
          clusterSize: 2      # HA
```

`region_suffix` is always the env name (like object storage). `vpc_id`/`subnet_id`/
`cluster_size` render as HCL numbers.

### Why `_v2` / VPC

`linode_database_postgresql_v2` (plugin-framework) is the only resource with the
`private_network` attachment. `public_access=false` is the whole point — a
Managed DB otherwise ships a public `g2a.akamaidb.net` endpoint (TLS-only, but
internet-reachable). VPC-only matches the platform's security posture.

## Migration: existing public cluster → VPC (gsap-apl `gsap-postgres`, id 490457)

The live `gsap-postgres` cluster is **public, non-VPC**, Aiven-platform
(`rdbms-default`), pg 18.4, `us-ord`, HA. It was created out-of-band and is not in
any TF state. Two paths:

**A. Greenfield + data migration (recommended).** Linode does **not** support
attaching an existing Managed DB to a VPC in place, and the VPC path provisions on
a different platform/network. So:
1. Apply the `databases` root → a new VPC-attached cluster (`platform-postgres-<env>`).
2. `pg_dump` from `gsap-postgres` (public, over TLS) → `pg_restore` into the new
   cluster from a bastion/job **inside the VPC** (the new cluster has no public IP).
   (Downstream logical DBs are Crossplane-provisioned and small; or dump per-app.)
3. Re-seed `secret/platform/db-admin` with the new cluster's admin creds
   (`llz ci seed-db-admin`), force-sync ESO, let Crossplane re-provision per-app
   DBs/roles against the new endpoint.
4. Cut consumers over (they read the endpoint from `gsap-db-endpoint`, which the
   ESO re-seed updates), verify, then delete 490457.

**B. Import (only if staying public).** `terraform import` 490457 into the
`databases` root and set `public_access=true` — but this keeps the public endpoint
and defeats the feature's purpose; not recommended.

The cutover is a deliberate, scheduled data migration — not a GitOps side effect.
Downstream `Database`/`Role`/`Grant` CRs are `deletionPolicy: Orphan`, so they
survive the endpoint change; only the admin secret + endpoint move.

## Remaining work (this branch)

- `tools/cmd/llz/ci_seed_dbadmin.go` — `llz ci seed-db-admin --region`: read the
  `databases` root's TF outputs (host/port/root_username/root_password/ca_cert),
  write `secret/platform/db-admin` (keys `endpoint/port/username/password/ca`,
  `sslmode=require`), idempotent via a `presentField` + `rotated_at` stamp (mirror
  `ci_mint_objkeys.go`); register in `ci.go`.
- Workflow: `apply-databases` / `plan-destroy-databases` / `destroy-databases` jobs
  in `llz-terraform.yml` (clone the object-storage jobs, `module: databases`); add
  `databases` to the `terraform.yml` module choice; add a "Seed DB admin" step to
  `llz-bootstrap-openbao.yml` (`needs: apply-databases`).
- `scaffold_spec.go` — emit a `databases:` block in scaffolded specs.
- `validate.go` — validate `databases` (region required when vpcId/subnetId set;
  cluster_size ∈ {1,2,3}).
- `docs/landing-zone-spec.md` / `docs/workflows/llz-terraform.md` — document the
  new spec fields + jobs.
