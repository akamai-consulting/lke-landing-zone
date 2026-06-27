# Design: in-cluster Linode credential rotator (CronJob)

**Status:** Draft / design-stage (skeleton below is not yet wired into any env).
**Item:** kube-native cred-hardening #4 (generalized) — move the rotation of every
long-lived **Linode-issued** credential out of CI and into the cluster.
**Relates to:** [secrets.md](../secrets.md), the `linode-volume-labeler` CronJob
(`apl-values/components/volumeLabeler/`), `internal/linode` (rotation primitives),
`credentials_pat.go` / `credentials_objkey.go` (existing orchestration), the
`secret-propagator` OpenBao policy (`tools/cmd/llz/ci_openbao_configure.go`).

## Problem

Several long-lived credentials in the platform are **minted by the Linode API**
and rotated **from CI** (`secret-rotation.yml` → `llz credentials …` →
`llz ci propagate-pat` → OpenBao), with a copy in `infra-<env>` GitHub secrets:

| Credential | GitHub secret | OpenBao path | Consumed by | Trust domain |
|---|---|---|---|---|
| Linode PAT | `LINODE_API_TOKEN` | `secret/linode/api-token` | `linode-volume-labeler` (in-cluster) **+ Terraform/CI** | **dual** |
| Loki S3 key | `LOKI_S3_*` | `secret/loki/object-store` | Loki (in-cluster, ESO) | **in-cluster only** |
| Harbor registry S3 key | `HARBOR_REGISTRY_S3_*` | `secret/harbor/registry-s3` | Harbor registry (in-cluster, ESO) | **in-cluster only** |
| Gitea backup S3 key | `GITEA_BACKUP_S3_*` | (gitea backup) | Gitea backup job (in-cluster) | **in-cluster only** |
| DNS-scoped PAT | `LINODE_DNS_TOKEN` | `secret/certmanager/dns01` | cert-manager DNS-01 (in-cluster, ESO) | **in-cluster only** |

Every one of these is a Linode-minted credential whose in-cluster copy lives in
OpenBao and reaches consumers via ESO. The rotation loop is the same shape for
all of them — mint via the Linode API, write OpenBao, drain the old — yet it runs
in CI today. The kube-native target is **one in-cluster CronJob** that owns this
loop for all of them: no CI step, no `propagate-pat`, and (for the in-cluster-only
ones) no GitHub secret to keep in sync.

## Goals / non-goals

**Goals**
- Rotate every in-cluster Linode credential on a schedule, in-cluster, no CI.
- One table-driven rotator; reuse the existing `internal/linode` primitives.
- **No GitHub App / no GitHub write dependency** for the in-cluster-only
  credentials (see the trust-domain spectrum) — so this lands independently of
  cred-hardening #3.
- Least-privilege per credential (bucket-scoped OBJ keys; DNS-only PAT).
- Verify-before-revoke so a bad mint can never break a consumer.

**Non-goals**
- Rotating credentials Terraform/CI consume out-of-cluster (the `TF_STATE_*` OBJ
  key — foundational/circular — and the broad provisioning PAT; see below).
- A controller-runtime Deployment. Rotation is periodic → a CronJob (the
  `linode-volume-labeler` shape) is the right tool.

## The trust-domain spectrum (what's clean vs what needs care)

The credentials sort into three tiers — **do the clean tier first.**

1. **In-cluster-only (cleanest — Phase 1).** Loki / Harbor-registry / Gitea-backup
   S3 keys + the DNS token. Terraform mints these once to create the bucket /
   provision DNS, but **post-bootstrap only in-cluster workloads consume them**
   (via ESO from OpenBao). The rotator fully owns them: mint → OpenBao → drain,
   with **no GitHub secret to update and no out-of-cluster reader**. The
   `infra-<env>` GitHub copy becomes bootstrap-only (vestigial after first boot).
   No dual-trust-domain tension, no GitHub App.

2. **Dual-domain (needs a decision — Phase 2).** `LINODE_API_TOKEN` is read both
   in-cluster (volume-labeler) **and** by Terraform/CI (broad provisioning scopes,
   + the `infra-<env>` GitHub secret). A purely in-cluster rotator can own the
   OpenBao copy but **cannot** update the GitHub secret without GitHub write (a
   stored PAT or the #3 GitHub App). **Recommendation: split the credential** —
   mint a narrow, in-cluster-only PAT (Volumes RW) the rotator owns end-to-end,
   and leave the broad provisioning PAT as-is (CI-rotated). That collapses the PAT
   into tier 1 and keeps #4 independent of #3.

3. **Out of scope.** The `TF_STATE_*` OBJ key backs the Terraform state the
   cluster is built from — rotating it in-cluster is circular. Stays CI/manual.

## Proposed design

A per-env **CronJob** (modeled on `linode-volume-labeler`) running a new
`llz ci rotate-linode-creds` subcommand driven by a **declarative table** of
managed credentials. Each table entry:

```
{
  kind:        pat | objkey,
  label:       Linode resource label (also the drain target, keep-newest-N),
  scopes:      Linode scopes string,                            # pat
  cluster/bucket/perms:  bucket-scoped key parameters,          # objkey
  baoPath:     OpenBao KV path to write,
  baoFields:   how the minted result maps to KV fields
               (pat -> {token}; objkey -> {access_key_id, secret_access_key, …}),
  rotateAfter: days,
  keepN, grace,
}
```

Per run, for each entry that is **due** (newest existing resource of that label is
older than `rotateAfter`):

1. **Mint** via the Linode API (`CreateProfileToken` / `CreateObjectStorageKey` —
   both already exist), authenticating with the in-cluster `LINODE_API_TOKEN`
   (ESO-synced Secret).
2. **Verify** the new credential works (PAT: `GET /v4/profile`; OBJ key: a cheap
   S3 HEAD against the bucket) BEFORE touching anything else.
3. **Write** it to the entry's OpenBao path via a Kubernetes-auth role bound to a
   write-scoped policy. ESO syncs it to consumers within its `refreshInterval`.
4. **Drain** older resources of that label past the grace window
   (`DeleteProfileToken` / `DeleteObjectStorageKey`, keep-newest-N) — never the one
   just written, never before the new one is confirmed in OpenBao.

Verify-before-revoke + keep-newest-N means a failed mint or bad credential leaves
the previous working one live; the next run retries. The PAT and OBJ-key paths are
the same control flow over different primitives — hence one table-driven command.

The rotation orchestration already exists as `llz credentials pat|obj-key
create|revoke-old`; the new command reuses that logic, dropping the GitHub-secret
write (tier-1 creds don't need it) and adding the verify probe + the k8s-auth
OpenBao write.

### Components (skeleton — `apl-values/components/linodeCredRotator/`)

Mirror `volumeLabeler/linode-volume-labeler/`:

- **`namespace.yaml`** — `llz-linode-cred-rotator`, PSS-restricted.
- **`externalsecret.yaml`** — ESO syncs `secret/linode/api-token` →
  `linode-api-token` Secret (the *minting* credential the rotator authenticates
  with).
- **`cronjob.yaml`** — daily (rotates only when due), `concurrencyPolicy: Forbid`,
  runs `llz ci rotate-linode-creds`.
- **`rbac.yaml`** — ServiceAccount + TokenRequest for OpenBao k8s-auth login.
- **`network-policy.yaml`** — egress to the Linode API + S3 endpoints (443),
  OpenBao (`platform-openbao:8200`), DNS, K8s API (LKE-E 443→6443).
- **`kustomization.yaml`** — `kind: Component`, gated on
  `spec.components.linodeCredRotator`.

### OpenBao auth (write path)

The rotator writes several KV paths, so it needs a dedicated write policy (the
existing `secret-propagator` only covers `linode/api-token`). Add a
`linode-rotator` policy + a Kubernetes-auth role (in `baoConfigureSteps`,
alongside `eso` / `eso-pusher`):

```hcl
# policy linode-rotator
path "secret/data/linode/api-token"      { capabilities = ["create","update","read"] }
path "secret/data/loki/object-store"     { capabilities = ["create","update","read"] }
path "secret/data/harbor/registry-s3"    { capabilities = ["create","update","read"] }
path "secret/data/certmanager/dns01"     { capabilities = ["create","update","read"] }
# + metadata read for each
```

```go
{desc: "write kubernetes auth role linode-rotator", fatal: true,
    args: []string{"write", "auth/kubernetes/role/linode-rotator",
        "bound_service_account_names=linode-cred-rotator",
        "bound_service_account_namespaces=llz-linode-cred-rotator",
        "policies=linode-rotator", "ttl=15m"}},
```

(Paths are enumerated, not wildcarded — same discipline as `platform-ci`.)

## Bootstrap / cold-start

Each credential's **first** value is still seeded once at bootstrap (the workload
can't start without it, and the rotator needs the minting `LINODE_API_TOKEN` to
exist). The rotator takes over on its first due run. Per-cluster, standalone/HA.

## Failure modes

| Failure | Behavior |
|---|---|
| Mint fails (API/quota) | old credential untouched; CronJob fails; next run retries; rotation-age SLA alerts if overdue |
| New credential doesn't work | verify probe fails before any write/revoke → old stays live |
| OpenBao write fails after mint | new credential exists but unused → drained by keep-newest-N next run |
| Partial drain | keep-newest-N is idempotent; re-runs converge |
| Self-lockout (incl. the minting PAT) | impossible — old is revoked only after the new one is verified AND written |

## Observability

A rotation-age SLA check per managed credential (reuse
`health.ClassifyRotationAge`, like `health-loki-objkey-rotation`), surfaced by
`llz-scheduled-checks.yml` — replacing the in-cluster credentials' share of the
CI rotation-health.

## What this retires (once enabled + e2e-validated)

- The in-cluster credentials' slices of `secret-rotation.yml`
  (`credentials pat` / `credentials obj-key` create+revoke-old) and
  `llz ci propagate-pat` — the rotator writes OpenBao directly.
- The `infra-<env>` GitHub-secret copies of the **in-cluster-only** credentials
  become bootstrap-only (vestigial after first boot).
- NOT retired: the broad provisioning PAT's CI rotation and the `TF_STATE_*` key.

## Rollout (phased — clean tier first)

1. Land this design + the skeleton component (disabled; not in any env).
2. **Phase 1 — in-cluster-only creds:** implement `rotate-linode-creds` for the
   OBJ keys (Loki, Harbor-registry, Gitea-backup) + the DNS token (+ unit tests
   over the pure due / keep-newest-N / verify logic) and the `linode-rotator`
   policy/role. No GitHub App, no dual-domain. Enable in lab/e2e; confirm a real
   cycle (mint → verify → OpenBao → ESO → consumer still works → drain).
3. **Phase 2 — the PAT:** after the tier-2 split decision (narrow in-cluster PAT
   vs single token + #3), add the PAT entry to the same table.
4. Retire the in-cluster credentials' CI rotation steps.

## Open questions

- **PAT: split vs single token + #3.** Phase 1 sidesteps this entirely (OBJ keys
  + DNS token have no dual-domain). The split is only needed to bring the PAT into
  the rotator without depending on #3.
- **Gitea-backup S3 key** — confirm it's consumed in-cluster (this repo) vs
  apl-core before adding it to the table.
- **Grace / keep-N / rotateAfter** — align with the existing `credentials`
  defaults (PAT 90-day validity; OBJ keys keep-newest, drain past grace).
- **CronJob vs operator** — CronJob chosen for simplicity.
