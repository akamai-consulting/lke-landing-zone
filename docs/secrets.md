# Secrets — OpenBao operations guide

This document is the runbook for the platform's secret backend. It covers:

- What OpenBao is and why this template chose it (summary; brief rationale inline below)
- Initial per-region cluster bring-up (init, unseal, KV v2, AppRole)
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

- `ha_role = "active"` — provisions Harbor robots, owns the base-named AppRole
  GH-secret (`OPENBAO_APPROLE_SECRET_ID`), and receives its standby peer's CA.
- `ha_role = "standby"` — mirrors the active: seeds Harbor creds from the
  active's published secrets, owns `OPENBAO_APPROLE_SECRET_ID_STANDBY`, and
  ships its CA to the active. Pairs share one `ha_group`.
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
for each region. The workflow handles the full bootstrap: Raft init, unseal, KV v2
setup, AppRole configuration, all secret seeding, and GitHub secrets population. See
[`docs/runbooks/bootstrap-openbao.md`](runbooks/bootstrap-openbao.md) for the
step-by-step procedure and required secrets.

The remainder of this section documents what the workflow does internally, which is
useful context for emergency recovery and understanding the secret layout.

### What bootstrap-openbao.yml does (reference)

1. **Initialize Raft** — `bao operator init -key-shares=5 -key-threshold=3`. Stores unseal keys 1–3 as `OPENBAO_UNSEAL_KEY_1/2/3` in the `infra-<deployment>` environment (one of `infra-primary`, `infra-secondary`, `infra-staging`, `infra-lab`); prints all 5 keys + root token to the job summary.

2. **Unseal** — unseals pod 0 with 3 keys; Raft-joins and unseals pods 1–2.

3. **Configure** — enables KV v2 at `secret/`, AppRole auth, Kubernetes auth. Creates the CI policy (read-only, paths enumerated explicitly — no wildcard) and AppRole role. Pins `role_id=<instance>-ci`. Enables the file audit device.

4. **Seed secrets** — writes the following into OpenBao KV v2 and sets the corresponding GitHub secrets:
   - `eso-approle-secret` K8s Secret (ESO ClusterSecretStore auth)
   - `OPENBAO_APPROLE_ROLE_ID` / `OPENBAO_APPROLE_SECRET_ID[_STANDBY]` GitHub secrets
   - `secret/harbor/admin` (Harbor admin password)
   - `secret/harbor/robot` (Harbor CI robot credentials, push+pull+delete; stored for buildah builds via `harbor/docker-config`)
   - `secret/harbor/pull-robot` (Harbor pull-only robot credentials; distributed as imagePullSecret to kube-system and workload namespaces)
   - `secret/harbor/docker-config` (Docker config JSON for buildah cert-automation builds)
   - `secret/infra/github-dispatch-token` (harbor-ready PostSync dispatch)
   - `secret/approle/rotation-secrets` (AppRole rotation CronWorkflow)
   - `secret/cert-automation/github-token` (cert-automation Argo Workflow)
   - `secret/loki/object-store` (Linode Object Storage keys from `LOKI_S3_ACCESS_KEY/SECRET`)
   - Note: `secret/grafana/admin` (admin credentials) and `secret/otel/ingress` (OTLP bearer) are NO LONGER seeded here — they are generated in-cluster by External Secrets Operator (a Password generator + a PushSecret with `updatePolicy: IfNotExists`) and written into the same OpenBao paths. See `apl-values/_shared/manifest/generated-secrets/`.
   - Note: `secret/certmanager/dns01` (Linode DNS token from `LINODE_DNS_TOKEN`) is seeded by the separate `bootstrap-dns.yml` workflow once a DNS-scoped token has been provisioned.

5. **Revoke root token** — runs unconditionally even on failure.

### Emergency manual bring-up (fallback only)

If the workflow fails partway through and you need to intervene manually, the
equivalent `bao` commands can be found in the workflow source
(`instance-template/.github/workflows/bootstrap-openbao.yml`). Access OpenBao via
`kubectl exec` into the `<release>-openbao-0` pod with
`VAULT_ADDR=https://127.0.0.1:8200 VAULT_SKIP_VERIFY=true`.

## Secret layout

The table below covers the operator-managed application secrets. Infrastructure
secrets seeded by `bootstrap-openbao.yml` (Harbor credentials, Grafana admin, OTel
bearer token, Loki object-store keys, etc.) are listed in the
[Initial cluster bring-up](#initial-cluster-bring-up) section above.

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

> **Known limitation — Loki admin password.** The Loki gateway's HTTP basic-auth
> admin password (`LOKI_ADMIN_PASSWORD`) is **not yet** on the ESO+OpenBao rotation
> lifecycle the other support-plane credentials use. The `llz-terraform` workflow's
> `llz ci ensure-env-secret` step generates it once and persists it to the
> `infra-<region>` GitHub environment secret on the first apply, then exports it as
> `TF_VAR_loki_admin_password`; later runs reuse the stored value — but it is never
> rotated, and `cluster-bootstrap` keeps no copy in TF state. Moving it onto the
> ESO+OpenBao path is a tracked follow-up.

## Writing / rotating secrets — dual-write

Use `llz openbao set`.
It writes to both regional clusters, verifies the SHA-256 hash of the post-write
payload matches, and rolls back the primary if the secondary write fails. It
**dry-runs by default** — add `--yes` to execute the write.

Prerequisites:

```bash
# Operator-level token for each region. Do NOT use the CI AppRole (<instance>-ci) — it is read-only.
# Obtain via your normal operator auth method, or via `bao operator generate-root` for
# emergency access (requires three of the five unseal key holders).
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
| CI AppRole activity                  | `{app="openbao", component="audit"} \|= "auth/approle/login" \| json` |
| Root-token usage (should be rare)    | `{app="openbao", component="audit"} \| json \| auth_display_name="root"` |

Region is exposed as the `region` label on each stream, set by the chart's Promtail
config. Override per cluster via the per-env Argo CD Application value overrides.

### Loki backend

Loki runs in the `observability` namespace on each regional cluster (apl-core
managed), with storage on **Linode Object Storage** (chunks + index) and **Linode
Block Storage** (WAL only). See [docs/playbooks/loki-access.md](playbooks/loki-access.md)
for access and bucket/credentials setup.

## Unseal automation

Today, every pod restart requires a manual unseal quorum: three of the five key
holders must each supply their share. Options that would automate this:

- **Auto-unseal via Transit** — a third, small "seal" OpenBao cluster that holds the root-of-trust key; all workload clusters use Transit to unseal automatically. Adds a new dependency but removes human unseal toil.
- **Auto-unseal via cloud KMS** — OpenBao supports cloud KMS providers. Not available on Linode today.

Until an auto-unseal path is wired up, keep the five-share threshold and follow the
runbook for humans to unseal after any pod restart.

## Cross-references

- `llz openbao set` — dual-write implementation
- `llz openbao get` — CI read helper
- [docs/architecture/convergence-contract.md](architecture/convergence-contract.md) — cluster convergence contract
- [docs/runbooks/bootstrap-openbao.md](runbooks/bootstrap-openbao.md) — OpenBao bring-up procedure
- [docs/runbooks/approle-rotation.md](runbooks/approle-rotation.md) — CI AppRole rotation
- [docs/runbooks/lke-admin-rotation.md](runbooks/lke-admin-rotation.md) — lke-admin credential rotation
- [docs/runbooks/linode-credential-rotation.md](runbooks/linode-credential-rotation.md) — Linode token rotation
- [docs/runbooks/apl-values-propagation.md](runbooks/apl-values-propagation.md) — apl-values propagation
- [docs/playbooks/openbao-accounts.md](playbooks/openbao-accounts.md) — OpenBao account/access management
- [docs/playbooks/operator-onboarding.md](playbooks/operator-onboarding.md) — day-2 operator onboarding
- [docs/alerting.md](alerting.md) — alerting and on-call
- [docs/adopter-guide.md](adopter-guide.md) — standing up your own instance
