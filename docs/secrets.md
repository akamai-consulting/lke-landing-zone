# Secrets — OpenBao operations guide

This document is the runbook for the platform's secret backend. It covers:

- What OpenBao is and why this template chose it (summary; brief rationale inline below)
- Initial per-region cluster bring-up (init, auto-unseal, KV v2, auth methods)
- Rotating secrets via the dual-write script
- How CI reads secrets at deploy time
- Regional failover behavior

The secret store itself runs as an Argo CD-managed Helm release of the published
`llz-openbao-platform` chart; its Argo CD Application + manifests live ONCE in the
shared `instance-template/apl-values/components/openbao/` component (enabled per env
via `spec.components.openbao`), and per-env value overrides in `apl-values/<env>/values.yaml`.
Your application workloads never talk to OpenBao directly — CI fetches the values
it needs and injects them as deploy-time configuration. (For an edge/serverless
target, for example, that means CI reads the values and passes them to the deploy
tool as variables; the mechanism is "CI injects at deploy time", not a runtime
client of OpenBao.)

## Topology

```
  Region: primary  (e.g. us-lax)              Region: secondary  (e.g. us-sea)
  ┌────────────────────────────────┐          ┌────────────────────────────────┐
  │  LKE Enterprise cluster        │          │  LKE Enterprise cluster        │
  │  - Argo CD                     │          │  - Argo CD                     │
  │  - Prometheus / Grafana / OTel │          │  - Prometheus / Grafana / OTel │
  │  - OpenBao HA (3-node Raft)    │          │  - OpenBao HA (3-node Raft)    │
  └──────────────┬─────────────────┘          └────────────────┬───────────────┘
                 │                                             │
                 │   no automatic replication                  │
                 │   between these two clusters                │
                 └──────────────┐              ┌───────────────┘
                                ▼              ▼
                          ┌──────────────────────────────────────┐
                          │  Operator / CI runner                │
                          │  llz openbao set  │  (dual-write, or single-write if standalone)
                          │  llz openbao get  │  (read by role: active | standby)
                          └──────────────────────────────────────┘
```

Application workloads may run off-cluster (e.g. on an edge/serverless target) — in
that case the LKE clusters are the support plane only, holding the secret backend
and observability stack.

### HA roles are declared, not hardcoded

The active/standby relationship above is **declared per deployment** in its
cluster tfvars, not baked into "primary"/"secondary" strings:

- `ha_role = "active"` — provisions Harbor robots and receives its standby
  peer's CA.
- `ha_role = "standby"` — mirrors the active: seeds Harbor creds from the
  active's published secrets and ships its CA to the active. Pairs share one
  `ha_group`.
- `ha_role = "standalone"` (the default) — a single self-contained OpenBao. No
  peer, no cross-region CA, Harbor provisioned locally, and `llz openbao set`
  **single-writes** (no standby to dual-write to).

`llz env role <deployment>` and `llz env peer <deployment>` resolve these; the
bootstrap, rotation, and Harbor workflows branch on the role/peer instead of the
deployment name. `llz openbao get/set` addresses clusters by role
(`OPENBAO_ADDR_ACTIVE` / `OPENBAO_ADDR_STANDBY`).

## Why OpenBao and not Vault OSS

Short version: Apache 2.0 vs BSL 1.1, Linux Foundation governance, near-identical
feature set for this template's scope. OpenBao also ships an officially HA Postgres
storage backend that Vault OSS does not — not used today but available as a future
option. This is why OpenBao is the secret backend of record for the platform.

## Why operator-side dual-write and not a stretched cluster

OpenBao OSS has no cross-region replication primitive. Performance Replication is a
Vault Enterprise feature, intentionally not ported into OpenBao. The choices were:

1. **Stretched Raft cluster across regions (5 nodes, 3 in one region, 2 in the other)** — every write crosses the inter-region link; loses quorum if the majority region fails. Rejected.
2. **Two independent HA clusters + operator-side dual-write** — near-zero-write workload makes this trivial; regional failover is a config change, not a Raft recovery operation. **Chosen.**

## Initial cluster bring-up

**This process is automated.** Run `instance-template/.github/workflows/bootstrap-openbao.yml`
for each region. The workflow handles the full bootstrap: Raft init, auto-unseal, KV v2
setup, auth-method configuration (Kubernetes auth + GitHub-OIDC), all secret
seeding, and GitHub secrets population. See
[`docs/runbooks/bootstrap-openbao.md`](runbooks/bootstrap-openbao.md) for the
step-by-step procedure and required secrets.

The remainder of this section documents what the workflow does internally, which is
useful context for emergency recovery and understanding the secret layout.

### What bootstrap-openbao.yml does (reference)

1. **Seed the static seal key + Initialize Raft** — first creates the 32-byte static auto-unseal key as the `openbao-unseal-key` Secret (so the pods can start and unseal themselves; the key is also persisted as `OPENBAO_SEAL_KEY` in the `infra-<deployment>` environment for disaster recovery and must be copied offline — losing it loses the data). Then runs `bao operator init -recovery-shares=5 -recovery-threshold=3`. Stores recovery keys 1–3 as `OPENBAO_RECOVERY_KEY_1/2/3` in the `infra-<deployment>` environment (one of `infra-primary`, `infra-secondary`, `infra-staging`, `infra-lab`); prints all 5 recovery keys + root token to the job summary. The recovery keys authorize `bao operator generate-root` / `rekey` — they do **not** unseal (the static seal key does) and **cannot** decrypt the root key.

2. **Auto-unseal** — each pod unseals itself at boot from the static seal key (`seal "static"` in the chart); the workflow waits for all 3 to converge to unsealed (followers join the leader via Raft `retry_join`). There is no manual key submission.

3. **Configure** — enables KV v2 at `secret/`, Kubernetes auth, and GitHub-OIDC (`jwt`) auth. Creates four least-privilege policies (paths enumerated explicitly — no wildcard):
   - the read-only `platform-ci` policy, bound to the `eso` Kubernetes-auth role, which every in-cluster consumer reads through;
   - the write-scoped `eso-pusher` policy, bound to the `eso-pusher` Kubernetes-auth role (same ESO controller SA as `eso`), for the in-cluster-sourced PushSecret paths (`grafana/admin`, `otel/ingress`, `harbor/admin`);
   - the `linode-rotator` policy, bound to the `linode-rotator` Kubernetes-auth role, for the in-cluster credential rotator's paths (`loki/object-store`, `harbor/registry-s3`);
   - the `secret-propagator` GitHub-OIDC role + policy used by `llz ci propagate-pat`.

   Enables the file audit device.

4. **Seed secrets** — writes the following into OpenBao KV v2 and sets the corresponding GitHub secrets:
   - `secret/harbor/robot` (Harbor CI robot credentials, push+pull+delete; the buildah `config.json` is derived from these in-cluster by ESO — see note below)
   - `secret/harbor/pull-robot` (Harbor pull-only robot credentials; distributed as imagePullSecret to kube-system and workload namespaces)
   - `secret/infra/github-dispatch-token` (harbor-ready PostSync dispatch)
   - `secret/cert-automation/github-token` (cert-automation Argo Workflow)
   - `secret/loki/object-store` (Linode Object Storage keys minted at bootstrap by `llz ci mint-bootstrap-objkeys`, rotated by the in-cluster linodeCredRotator)
   - Note: `secret/harbor/admin`, `secret/grafana/admin` and `secret/otel/ingress` are NO LONGER seeded here — External Secrets Operator writes them in-cluster via PushSecrets (harbor mirrors its Helm-generated Secret; grafana/otel use a Password generator + `updatePolicy: IfNotExists`), through the write-scoped `openbao-push` store. See `apl-values/components/harbor/` and `apl-values/_shared/manifest/generated-secrets/`.
   - Note: `secret/harbor/docker-config` is NO LONGER seeded — the buildah `config.json` is derived in-cluster by the `llz-cert-automation` chart's `harborDockerConfig` ExternalSecret, which renders the dockerconfigjson from the robot creds (`username`/`password`/`registry_host`) in `secret/harbor/robot` via an ESO template.

5. **Revoke root token** — runs unconditionally even on failure.

### Emergency manual bring-up (fallback only)

If the workflow fails partway through and you need to intervene manually, the
equivalent `bao` commands can be found in the workflow source
(`instance-template/.github/workflows/bootstrap-openbao.yml`). Access OpenBao via
`kubectl exec` into the `<release>-openbao-0` pod with
`VAULT_ADDR=https://127.0.0.1:8200 VAULT_SKIP_VERIFY=true`.

## Secret layout

The table below covers the operator-managed application secrets. Infrastructure
secrets seeded by `bootstrap-openbao.yml` (Harbor robot credentials, Loki
object-store keys, etc.) are listed in the
[Initial cluster bring-up](#initial-cluster-bring-up) section above; the paths that
are instead sourced or rotated in-cluster are covered in
[In-cluster rotation lifecycle](#in-cluster-rotation-lifecycle) below.

| Path                          | Keys                          | Writer   | Reader      |
|-------------------------------|-------------------------------|----------|-------------|
| `secret/<project>/keys`       | `<app_secret>`                | Operator | Operator (drift check via `llz openbao get`) |
| `secret/<project>/config`     | `<app_config_value>`          | Operator | Operator (drift check via `llz openbao get`) |
| `secret/<project>/<workload>` | `<workload_private_pem>`      | Operator | Workload pod (via ExternalSecret → K8s Secret mount) |

> **Note:** CI (deploy) reads the application secrets directly from **GitHub Actions
> environment secrets** (`lab`, `staging`, `production`) — not from OpenBao at deploy
> time. OpenBao holds the canonical copy for operator-side dual-write consistency and
> audit. Keep both in sync: after any `llz openbao set` rotation, also update the
> corresponding GitHub environment secrets and re-run the deploy workflow.

Any secret that must be **identical across both regions** (for example, a shared key
seed that every instance derives the same configuration from) must be kept in sync:
if primary and secondary drift, requests routed to the drifted region fail.

### In-cluster rotation lifecycle

Two Linode-minted support-plane credentials are rotated **in-cluster** — no CI
step, no GitHub secret — by the `linodeCredRotator` CronJob (`llz ci
rotate-linode-creds`; see [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md)
and [docs/designs/linode-credential-rotator.md](designs/linode-credential-rotator.md)):

- `secret/loki/object-store` — Loki's Object Storage keys
- `secret/harbor/registry-s3` — Harbor registry's Object Storage keys

For each, when the OpenBao `rotated_at` stamp is older than the threshold (or absent
on a fresh seed), the rotator mints a replacement via the Linode API, **verifies it
before touching the old one**, writes it to OpenBao through the `linode-rotator`
Kubernetes-auth role, then drains older same-labeled resources (keep-newest-N).
`bootstrap-openbao.yml` seeds `secret/loki/object-store` once; the rotator adopts it
on its first run and owns it thereafter. `secret/harbor/registry-s3` is **not** seeded
at bootstrap — the rotator creates it on first run.

> **DNS-01 note.** cert-manager DNS-01 challenges are solved by apl-core's
> `cert-manager-webhook-linode` (API group `acme.slicen.me`), which holds its own
> static Linode API token supplied via `TF_VAR_linode_dns_token` (from the
> `LINODE_DNS_TOKEN` GitHub secret) as `apps.cert-manager.dns.provider.linode.apiToken`.
> The landing zone no longer seeds a separate `secret/certmanager/dns01` OpenBao path;
> the `llz-letsencrypt-*` ClusterIssuers target that webhook.

Separately, three secrets are **sourced in-cluster** by ESO PushSecrets (not minted
by the rotator) and pushed up to OpenBao through the `eso-pusher` Kubernetes-auth
role: `secret/grafana/admin` and `secret/otel/ingress` (generated once via a Password
generator, `updatePolicy: IfNotExists`) and `secret/harbor/admin` (mirrored from
Harbor's Helm-generated Secret).

> **Loki admin password — apl-core-managed (6.x).** The Loki gateway admin
> password is no longer a landing-zone secret. On apl-core 6.x the
> `apps.loki.adminPassword` values field is an x-secret with a generator
> (`x-secret: '{{ randAlphaNum 20 }}'`), and the loki reverse-proxy auth Secret is
> an ExternalSecret sourced from apl-core's own `core-secrets-store` — so apl-core
> generates, persists, and self-wires the password in-cluster when it is omitted.
> The landing zone no longer supplies it: there is no `LOKI_ADMIN_PASSWORD` GitHub
> environment secret, no `TF_VAR_loki_admin_password`, and no `ensure-env-secret`
> step. (On 5.0.0 the x-secret had no generator, so it had to be supplied via
> Terraform — see [docs/designs/apl-core-v6-migration.md](designs/apl-core-v6-migration.md).
> Nothing on the landing-zone side consumes this password — only apl-core's loki.)

### Secret & token inventory

Every credential the platform manages and how it is rotated. (Non-secret config
variables — `TF_IMAGE`, `KUBE_IMAGE`, `TF_STATE_BUCKET/ENDPOINT`, `HARBOR_URL`,
`E2E_*` — are omitted.) "Rotation method" legend: **automated** (workflow/CronJob on a
cadence), **on-demand** (operator-triggered workflow), **manual** (operator action,
policy SLA), **generate-once** (created in-cluster, not re-rotated), **ephemeral**
(short-TTL, minted per use), **static** (never rotated by design).

**GitHub Actions secrets** (operator/CI-managed; `infra-<env>` scope unless noted):

| Secret | What it is | Rotation method |
|--------|------------|-----------------|
| `LINODE_API_TOKEN` | Linode provisioning PAT (read/write) | **Automated** — `secret-rotation.yml` mints + propagates monthly (`0 4 1 * *`), revokes old daily (`30 3 * * *`); ≤90-day policy with daily expiry audit |
| `LINODE_DNS_TOKEN` | Linode API token for apl-core's `cert-manager-webhook-linode` DNS-01 solver (`TF_VAR_linode_dns_token` → `apps.cert-manager.dns.provider.linode.apiToken`) | **Manual** — **static** operator input; ≤90-day policy |
| `CLOUD_FIREWALL_TOKEN` | Firewall-scoped PAT (optional) | **Manual** (Cloud Manager); ≤90-day policy |
| `TF_STATE_ACCESS_KEY` / `TF_STATE_SECRET_KEY` | Object Storage key for the TF-state backend bucket | **On-demand** via `secret-rotation.yml` (`tf-state-key` / `tf-state-key-revoke` scopes); no scheduled rotation (bootstrap dependency) |
| `OPENBAO_SECRETS_WRITE_TOKEN` | GitHub classic PAT (Actions + Secrets: write) | **Manual**; ≤90-day policy, daily `gh-pat-expiry` audit |
| `APL_VALUES_REPO_TOKEN` | GitHub fine-grained PAT (Contents: write) | **Manual**; ≤90-day policy, daily `gh-pat-expiry` audit |
| LKE admin kubeconfig | Cluster-admin credential | **Automated** — `secret-rotation.yml` (`lke-admin` scope), monthly; see [lke-admin-rotation.md](runbooks/lke-admin-rotation.md) |
| `E2E_DISPATCH_TOKEN` | GitHub classic PAT for the e2e harness (template-repo scope) | **Manual** (template-repo admin) |

**OpenBao KV v2 secrets** (`secret/…`):

| Path | What it holds | Rotation method |
|------|---------------|-----------------|
| `secret/linode/api-token` | Linode provisioning PAT | **Automated** — `secret-rotation.yml` → `propagate-pat` (GitHub-OIDC `secret-propagator` role) |
| `secret/loki/object-store` | Loki Object Storage keys | **Automated** in-cluster — `linodeCredRotator` (~80-day threshold) |
| `secret/harbor/registry-s3` | Harbor registry Object Storage keys | **Automated** in-cluster — `linodeCredRotator` (~80-day threshold) |
| `secret/grafana/admin` | Grafana admin password | **Generate-once** — ESO PushSecret, Password generator (`IfNotExists`) via `eso-pusher` role |
| `secret/otel/ingress` | OTel ingress bearer token | **Generate-once** — ESO PushSecret, Password generator (`IfNotExists`) via `eso-pusher` role |
| `secret/harbor/admin` | Harbor admin password | **Tracks Harbor** — ESO PushSecret mirrors Harbor's Helm-generated Secret (`Replace`) via `eso-pusher` role |
| `secret/harbor/robot` | Harbor CI robot (push/pull/delete) | **Static** — bootstrap seed; re-seed to rotate |
| `secret/harbor/pull-robot` | Harbor pull-only robot (imagePullSecret) | **Static** — bootstrap seed; re-seed to rotate |
| `secret/harbor/docker-config` | buildah `dockerconfigjson` | **Derived** — rendered in-cluster by ESO from `harbor/robot`; follows the robot creds (not seeded/stored) |
| `secret/cert-automation/github-token` | cert-automation Argo Workflow token | **Static** — bootstrap seed from `OPENBAO_SECRETS_WRITE_TOKEN`; follows that PAT |
| `secret/infra/github-dispatch-token` | harbor-ready PostSync dispatch token | **Static** — bootstrap seed from `OPENBAO_SECRETS_WRITE_TOKEN`; follows that PAT |

**OpenBao runtime auth & seal/recovery material:**

| Token / key | Lifetime | Rotation method |
|-------------|----------|-----------------|
| Kubernetes-auth tokens (`eso`, `eso-pusher`, `linode-rotator`) | 15m TTL | **Ephemeral** — minted per pod auth, auto-expires |
| GitHub-OIDC tokens (`platform-ci`, `secret-propagator`) | 15m TTL / 30m max | **Ephemeral** — minted per workflow run, auto-expires |
| `OPENBAO_ROOT_TOKEN` | Per bootstrap run | **Ephemeral** — revoked unconditionally at end of bootstrap; regenerated via recovery-key quorum |
| `OPENBAO_SEAL_KEY` | Permanent | **Static by design** — a changed key bricks auto-unseal; escrow offline |
| `OPENBAO_RECOVERY_KEY_1/2/3` | Permanent | **Static by design** — offline escrow; authorize `generate-root`/`rekey` quorum only |

Scheduled verification of these lives in `scheduled-checks.yml` (daily `0 6 * * *`):
Linode + GitHub PAT expiry audits (≤90-day policy, warn before expiry) and the
in-cluster rotation-SLA age checks. `secret-rotation.yml` carries the automated and
on-demand rotation jobs.

## Writing / rotating secrets — dual-write

Use `llz openbao set`.
It writes to both regional clusters, verifies the SHA-256 hash of the post-write
payload matches, and rolls back the primary if the secondary write fails. It
**dry-runs by default** — add `--yes` to execute the write.

Prerequisites:

```bash
# Operator-level token for each region. Do NOT use the ESO platform-ci credentials — they are read-only.
# Obtain via your normal operator auth method, or via `bao operator generate-root` for
# emergency access (requires three of the five recovery key holders).
export OPENBAO_ADDR_ACTIVE=https://openbao.primary.<cluster_domain>
export OPENBAO_ADDR_STANDBY=https://openbao.secondary.<cluster_domain>
export OPENBAO_TOKEN_ACTIVE=...        # operator token for the active cluster
export OPENBAO_TOKEN_STANDBY=...      # operator token for the standby cluster
```

Rotate a generated key seed:

```bash
new_seed=$(openssl rand -hex 32)
llz openbao set secret/<project>/keys <app_secret>="$new_seed" --yes
```

Set a config value:

```bash
llz openbao set secret/<project>/config <app_config_value>=<value> --yes
```

Provision a workload private key:

```bash
llz openbao set secret/<project>/<workload> \
    <workload_private_pem>="$(cat /secure/<workload>.pem)" --yes
```

After a successful dual-write, update the matching GitHub environment secrets and
trigger a redeploy:

```bash
gh workflow run <deploy-workflow>.yml --ref main
```

### Script behavior

| Scenario                              | Exit | Side effect |
|---------------------------------------|:----:|-------------|
| Both writes succeed, hashes match     | 0    | New version in both regions |
| Primary write fails                   | 2    | No change in either region |
| Secondary write fails                 | 3    | Primary rolled back to its prior version |
| Post-write hash mismatch              | 4    | Both regions updated, but inconsistent — manual intervention |
| Dry run (`SECRET_SET_DRY_RUN=1`)      | 0    | Nothing written; preview only |

The script uses KV v2's version history for rollback. If the prior version was 0 (no
secret existed), rollback is implemented as deleting the metadata path entirely so
the secret is fully removed.

### Drift verification

Run this any time as a consistency check:

```bash
hash_primary=$(
    llz openbao get active   secret/<project>/keys <app_secret> | shasum -a 256 | awk '{print $1}'
)
hash_secondary=$(
    llz openbao get standby secret/<project>/keys <app_secret> | shasum -a 256 | awk '{print $1}'
)
[[ "$hash_primary" == "$hash_secondary" ]] && echo "OK: in sync" || echo "DRIFT"
```

## CI read path

At deploy time the deploy jobs in your instance's deploy workflow read secrets from
**GitHub Actions environment secrets** (not directly from OpenBao). The environments
are `lab`, `staging`, and `production`; each holds its own copy of the application
secrets the workloads need (e.g. a key seed that must be identical across all
deployments in a region, plus any config values and workload private keys).

To rotate a runtime secret: update the value in the relevant GitHub environment
secrets and re-run the deploy workflow. For dual-region operator secrets that must
match across regions, update **all** environments so they stay in sync — there is no
automated cross-environment sync.

OpenBao holds the canonical copy of these values at `secret/<project>/...` and is the
write target for `llz openbao set`. The GitHub Actions
environment secrets and OpenBao values should be kept in sync manually after any
rotation.

## Regional failover

If the primary region is down:

1. **No action needed for CI** — GitHub Actions environment secrets are independent of regional OpenBao clusters; deploys continue to succeed.
2. **No action needed for the running workloads** — workloads deployed off-cluster have the last-pushed secrets cached as deploy-time variables; they keep serving.
3. **Rotation during an outage** — operators can continue `llz openbao set` only if **both** regions are up. During a primary outage, suspend rotations; when primary returns, run a drift check and, if needed, re-apply the last-written values.

This template intentionally does not support "write to secondary only during primary
outage" — that would create drift the moment primary returns, and there is no
automated reconciliation.

## Audit logging

OpenBao's audit device writes every authenticated request (including failures, and
including the caller identity of each write/read of a KV secret) as JSON to
`/openbao/audit/audit.log`.

Each OpenBao pod runs a **Promtail sidecar** (see the `llz-openbao-platform` chart's
`extraContainers`) that tails the audit log and ships events to the in-cluster Loki
instance in `observability`. The Promtail config is rendered by the chart into the
`<release>-openbao-promtail` ConfigMap.

### Enable the audit device (one-time, per region)

**This is automated by `bootstrap-openbao.yml`** and by the chart itself — the file
audit device is declared in the OpenBao HA config so it is present on every pod start
(the historical API-based `bao audit enable file ...` path is rejected by current
OpenBao with HTTP 400). To verify the device is active:

```bash
kubectl -n openbao exec -it <release>-openbao-0 -- \
    env VAULT_ADDR=https://127.0.0.1:8200 VAULT_SKIP_VERIFY=true VAULT_TOKEN=<token> \
    bao audit list
```

### Audit device failure stops OpenBao

OpenBao audit devices are **synchronous** — if the only enabled device cannot write
(disk full, volume gone, permission error), OpenBao stops servicing all requests
until the device recovers. Mitigations:

- Keep the audit storage sized with headroom and monitor usage.
- The Promtail sidecar is a reader of the file; it cannot block OpenBao's writer even if Loki is unreachable. This is why the sidecar pattern is preferred over a syslog device that ships directly.
- If you truly need non-blocking, enable a second audit device on a different path as an insurance policy:

  ```bash
  bao audit enable -path=file_backup file file_path=/openbao/audit/audit-backup.log
  ```

### Querying audit events

From Grafana (Loki data source), label set is `{app="openbao", component="audit"}`:

```
{app="openbao", component="audit"} | json
```

Common filters:

| What | LogQL |
|------|-------|
| All writes to `secret/<project>/*`   | `{app="openbao", component="audit"} \|= "request.path" \|= "secret/data/<project>" \| json \| request_operation="update"` |
| Failed authentications               | `{app="openbao", component="audit"} \| json \| error!=""` |
| ESO Kubernetes-auth activity         | `{app="openbao", component="audit"} \|= "auth/kubernetes/login" \| json` |
| Root-token usage (should be rare)    | `{app="openbao", component="audit"} \| json \| auth_display_name="root"` |

Region is exposed as the `region` label on each stream, set by the chart's Promtail
config. Override per cluster via the per-env Argo CD Application value overrides.

### Loki backend

Loki runs in the `observability` namespace on each regional cluster (apl-core
managed), with storage on **Linode Object Storage** (chunks + index) and **Linode
Block Storage** (WAL only). See [docs/playbooks/loki-access.md](playbooks/loki-access.md)
for access and bucket/credentials setup.

## Unseal automation

Pods **auto-unseal** at boot from a per-cluster 32-byte static seal key — no
managed KMS (none exists on Linode) and no human quorum on every pod restart. The
key is configured via OpenBao's `seal "static"` stanza in the chart
(`kubernetes-charts/llz-openbao-platform`), reading the key from the
`openbao-unseal-key` Secret mounted at `/openbao/seal/unseal.key`. The Secret lives
only in etcd, which LKE-Enterprise encrypts at rest, so the key satisfies the
"encrypt secrets at rest" control without a KMS.

The key is created by `llz ci bao-seed-seal-key` during bootstrap (before the pods
start) and persisted as the `OPENBAO_SEAL_KEY` `infra-<deployment>` environment
secret for disaster recovery: a lost namespace/Secret is restored from there, and
the same key re-unseals the existing Raft data. **Copy it to offline storage** —
the recovery keys from `bao operator init` authorize `generate-root` but cannot
decrypt the root key, so the static seal key is the only thing that can unseal the
data. The key is never rotated (a changed key bricks unseal); migrating an existing
Shamir-initialized cluster to static seal is out of scope (rebuild instead).

A cert-rotation or node replacement that restarts a pod no longer needs any manual
action — the pod re-reads the seal key and unseals itself. A persistently sealed
pod means the `openbao-unseal-key` Secret is missing/unreadable, the key is wrong,
or Raft storage is unhealthy.

## Cross-references

- `llz openbao set` — dual-write implementation
- `llz openbao get` — CI read helper
- [docs/architecture/convergence-contract.md](architecture/convergence-contract.md) — cluster convergence contract
- [docs/runbooks/bootstrap-openbao.md](runbooks/bootstrap-openbao.md) — OpenBao bring-up procedure
- [docs/runbooks/lke-admin-rotation.md](runbooks/lke-admin-rotation.md) — lke-admin credential rotation
- [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) — Linode token rotation
- [docs/runbooks/apl-values-propagation.md](runbooks/apl-values-propagation.md) — apl-values propagation
- [docs/playbooks/openbao-accounts.md](playbooks/openbao-accounts.md) — OpenBao account/access management
- [docs/playbooks/operator-onboarding.md](playbooks/operator-onboarding.md) — day-2 operator onboarding
- [docs/alerting.md](alerting.md) — alerting and on-call
- [docs/adopter-guide.md](adopter-guide.md) — standing up your own instance
