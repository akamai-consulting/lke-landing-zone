# `llz-secret-rotation.yml` — maintainer notes

`instance-template/.github/workflows/llz-secret-rotation.yml` is the reusable
(`workflow_call`) body of the monthly out-of-band rotation for the credentials
LLZ tracks against policy: the per-cluster `lke-admin` token, the broad Linode
PAT (`LINODE_API_TOKEN`) plus the narrow in-cluster PAT derived from it, and the
Terraform-state Object Storage key pair. The file is **vendored verbatim into
every customer instance by copier**, so it can never be updated in place once
shipped. This document is the archive for the design rationale, incident
history, and "why not X" reasoning that used to live in the workflow body; the
YAML keeps only the short notes a person needs while debugging a live run.

Surface-reduction pattern per
[`docs/adr/0003-vendor-actions-and-bodies-into-instances.md`](../adr/0003-vendor-actions-and-bodies-into-instances.md):
the instance ships a thin caller stub (owning the schedule crons + dispatch
inputs) and vendors this body plus the composite actions it calls.
`github.event_name` / `github.event.schedule` are inherited from the caller, so
the setup-job routing (schedule cron vs dispatch scope) is unchanged; dispatch
inputs are forwarded by the caller as `workflow_call` inputs.

---

## What the workflow rotates

### 1. `lke-admin-token`

Per LKE-Enterprise Guidelines, rotated via the Linode delete-kubeconfig API.
Per region: build the tool → fetch kubeconfig → resolve cluster ID → rotate
(the tool hard-asserts the cluster is LKE-Enterprise) → `terraform apply
-refresh-only` so the regenerated kubeconfig is repopulated into Terraform state
(the single propagation point for every CI consumer) → a bounded post-rotation
health gate.

### 2. `LINODE_API_TOKEN` (shared Linode PAT)

Label `gha-platform-platform_LINODE_API_TOKEN` — PAT-rotation policy
enforcement. Creates a new PAT and updates the GHA env secrets. **CI/Terraform-ONLY:**
the broad PAT is no longer pushed into any cluster. The previous PAT drains
daily via `linode-pat-revoke.yml` (companion workflow on a 7-day grace window).

Alongside it, each region's job mints the NARROW in-cluster PAT (label
`llz-incluster-<region>`, the token every in-cluster Linode consumer reads) and
writes it to that region's OpenBao (`secret/linode/api-token`) via
`llz ci rotate-incluster-pat`, which also drains older same-labeled siblings
past a 7-day grace window.

## Triggers

* **Monthly schedule** — runs both rotations across all regions.
* **`workflow_dispatch`** — `scope` chooses what to run (`lke-admin` /
  `linode-pat` / `all`). `lke-admin` emergencies still require `region` +
  a `rotate:<region>` confirmation; PAT runs require `rotate:linode-pat`; full
  runs require `rotate:all`. A non-empty `reason` is required for any dispatch.

## Auth model

The shared `LINODE_API_TOKEN` (the broad token Terraform uses) lives in
`infra-<region>` Environments, so `lke-admin` per-region rotations are gated by
Environment approval. The guidelines' recommendation of a Kubernetes-scoped PAT
is satisfied by the narrow in-cluster PAT — this is the closed tier-2 deviation:
in-cluster consumers no longer reuse the Terraform token. See
[`docs/designs/linode-pat-dns-consolidation.md`](../designs/linode-pat-dns-consolidation.md)
and [`docs/runbooks/lke-admin-rotation.md`](../runbooks/lke-admin-rotation.md).

## Why the job split looks redundant

GitHub scopes `environment:` per job, and a job can pin exactly one deployment.
The `create-*` / `revoke-*` jobs therefore pin a single deployment
(`infra-<first deployment>`) purely to read the environment-scoped secrets,
while `rotate` and `propagate-linode-pat` fan out over a region matrix with
`environment: infra-${{ matrix.region }}`. This is not duplication that can be
collapsed — merging the jobs would make the environment reference ambiguous.

---

## `workflow_call` interface

### `inputs.template-ref`

The template release this instance is rendered from — `llz upgrade` re-pins it.
Unused by this workflow's jobs (everything resolves locally); declared only
because the caller stub passes it and `workflow_call` rejects undeclared inputs.

### `secrets:` — all `required: false`

`TF_STATE_*` / `LINODE_API_TOKEN` are `infra-<env>` **environment** secrets,
resolved by the rotate jobs via their `environment: infra-<region>` block — they
are *not* passed across the `workflow_call` boundary (`secrets: inherit`
forwards only repo/org secrets).

Declaring them `required: true` makes GitHub reject the whole call (every job,
before any step) whenever they aren't repo-level — which is the case for
env-scoped instances, and always before the first cluster exists. `false` lets
the call through; the cluster-bound jobs then no-op on a missing cluster.

### Workflow-level `env:`

`TF_STATE_BUCKET` / `AWS_ENDPOINT_URL_S3` are identical across every job that
talks to the TF state backend (`rotate` + `propagate`); job-level `env` blocks
keep only what differs per job. `REGION` is matrix-derived, so it cannot live
here — the matrix context is not available at workflow level.

---

## Job: `discover`

Single source of truth for this rotation's per-deployment matrices — see
`llz-discover-deployments.yml`. `setup` consumes the `deployments` output for
the `lke-admin` `rotate` matrix, and `propagate-linode-pat` fans out over it
directly.

The scheduled health checks call the **same** reusable workflow, so the set of
deployments this rotation writes tokens into can never drift from the set those
checks verify.

## Job: `setup`

### Step: `Route scope + validate emergency confirmation`

Routes on `event_name` + (schedule cron expression OR dispatch scope) to the
outputs the downstream jobs consume:

| Output | Purpose |
| --- | --- |
| `run-lke-admin` | gate for the `rotate` matrix |
| `run-pat-create` | gate for `create-linode-pat` (+ `propagate-linode-pat`) |
| `run-pat-revoke` | gate for `revoke-linode-pat` |
| `run-tf-state-create` | gate for `create-tf-state-key` |
| `run-tf-state-revoke` | gate for `revoke-tf-state-key` |
| `regions` | JSON array consumed by the `rotate` matrix — the deployments discovered from the instance tfvars (`discover` job) on the monthly / `all` paths, or a single deployment on an `lke-admin` dispatch |
| `pat-apply` | passed to `linode-credentials` pat/create (true on schedule) |
| `revoke-apply` | passed to `linode-credentials` pat/revoke-old (true on schedule) |
| `tf-state-apply` | passed to `linode-credentials` obj-key/create (true on schedule) |
| `tf-state-revoke-apply` | passed to `linode-credentials` obj-key/revoke-old (true on schedule) |

The `DEPLOYMENTS` env var carries the deployments discovered from the instance
tfvars (`llz env list`). The monthly + `all` `lke-admin` rotations fan out over
this set — the same set the scheduled `lke-admin-rotation-health` check audits —
so every cluster that is checked is also rotated, and vice versa.

`llz` is baked into `TF_IMAGE`, so the routing step is just `llz ci rotation-plan`.

## Job: `rotate`

### `concurrency`

Serialized against `terraform.yml` / `release.yml` for the same region (group
`terraform-infra-<region>`) so the rotate→refresh window cannot race another
apply against stale state.

### Step: `Checkout the instance (TF roots)`

Single checkout: the instance at the workspace root — so the
`terraform-init` / `fetch-kubeconfig` actions' `terraform-iac-bootstrap/<module>`
working-dir and the rotate job's instance TF state both resolve, and the
composite actions resolve from this instance's own vendored `.github/actions/`.
(The same comment applied to the identical checkout step in every job.)

### Step: `Cluster access (kubeconfig + runner ACL)`

Shared cluster-access preamble: fetch the kubeconfig from TF state and open this
hosted runner's dynamic egress IP in the LKE-E control-plane ACL before any
`kubectl`; revoked at job end (`if: always()`).

`allow-missing: 'true'` — a deployment with no cluster yet (fresh instance, or a
peer that has not been provisioned) is a no-op, not a failure: there is nothing
to rotate until the cluster exists.

### Step: `No cluster yet — nothing to rotate`

`cluster-access` set `available=false`. This step makes the no-op explicit in the
run log / step summary and the cluster-bound rotation steps below are skipped.

### Step: `Rotate lke-admin`

The rotation prints its JSON audit record on stdout and appends it to the step
summary itself. This replaced the standalone `secret-rotation` binary plus an
inline `tee`.

### Step: `Refresh Terraform state (repopulate kubeconfig_raw)`

`delete-kubeconfig` regenerates the kubeconfig, invalidating the copy held in TF
state. `terraform apply -refresh-only` re-reads the new one from the LKE API into
state so every downstream consumer (`fetch-kubeconfig`) gets the live one. This
is the single propagation point — nothing else pushes the rotated kubeconfig.

### Step: `Post-rotation health gate`

Re-fetches the now-refreshed kubeconfig and waits (bounded, 360s / 15s interval)
for the control plane to accept it, because kubeconfig regeneration takes time to
sync.

## Job: `create-linode-pat`

The rotated token reaches downstream jobs via the just-rewritten GHA env secrets
(GHA refetches secrets at each job start) — **not** as a job output. The
`linode-credentials` (pat create) action registers `::add-mask::` on the value,
and the runner redacts masked values when serializing job outputs, so a job
output would arrive empty.

The broad PAT is no longer pushed into any cluster; the per-region
`propagate-linode-pat` matrix mints the narrow in-cluster PAT with it instead.

### `if:` guard

Skipped when no deployments exist yet (fresh instance) — `discover` emits `[]`,
and the `environment: infra-<first deployment>` below would otherwise be invalid.

### `environment:`

The account-wide `LINODE_API_TOKEN` and the `OPENBAO_SECRETS_WRITE_TOKEN` that
writes it back are `infra-<env>` **environment** secrets (the main-only injection
boundary), not repo-level — so they are read from the first deployment's
environment. `llz` then writes the rotated token into *every* deployment's
environment via `gha-secret-deployments`.

### `container:`

`llz` is baked into `TF_IMAGE` (`dockerfiles/Dockerfile`, `ci-terraform` target),
so the `linode-credentials` action needs no Go toolchain — it runs in `TF_IMAGE`.
The same applies to `revoke-linode-pat`, `create-tf-state-key`, and
`revoke-tf-state-key`.

### `env.GH_REPO`

`gh secret set` targets the instance repo on github.com (the default host).

### Step: `Rotate PAT + write the new token to each deployment's env secret`

`gha-secret-deployments` writes the rotated token into `LINODE_API_TOKEN` for
every deployment's `infra-<name>` environment (the env-scoped copies the
workflows read).

**Scopes.** The `scopes:` string must cover a full apply **and** a full destroy
(including orphan sweeps), or the rotated token silently re-introduces the
destroy failures we keep chasing:

* `events:read_only` — the cluster-delete waiter polls `/account/events`; its
  absence is the recurring `[401]` that aborts `terraform destroy` mid-teardown
  (and leaks the orphans that later quota-block the next apply).
* `volumes:read_write` — orphan block-volume sweep (TF destroy-time provisioner,
  `llz ci reap-volumes`, `llz ci preflight`).
* `nodebalancers:read_write` — orphan CCM NodeBalancer cleanup
  (`llz ci reap-nodebalancers`).

The rotator plumbs this string straight to `/v4/profile/tokens` with no
allowlist, so anything listed there is granted verbatim.

## Job: `propagate-linode-pat`

Replaces the old broad-PAT propagation. Each region's job mints its **own**
narrow in-cluster PAT (label `llz-incluster-<region>`) using the broad token,
writes it to that region's OpenBao, and drains older same-labeled siblings. The
narrow token never crosses a job boundary and never touches a GitHub secret.

The old propagate flow had to round-trip the broad PAT through the GHA secret
precisely because `::add-mask::`'d values cannot ride job outputs.

### `if:` guard — two entry paths

1. **Normal:** `create-linode-pat` just succeeded (full rotation) — the fresh
   broad PAT mints the narrow one.
2. **Recovery (`linode-pat-propagate-only`):** `create-linode-pat` is skipped; we
   mint a fresh narrow PAT with whatever broad token `secrets.LINODE_API_TOKEN`
   currently holds.

`always()` is required in the condition so that the *skipped* `create` dependency
does not auto-skip this job on the recovery path.

### `permissions.id-token: write`

Mints a GitHub Actions OIDC token to authenticate to OpenBao's `jwt` auth (role
`secret-propagator`). This replaced the long-lived AppRole `secret_id` that used
to be stashed in this environment's secrets and rotated in-cluster via
`gh secret set`. `llz ci rotate-incluster-pat` exchanges the OIDC token for a
short-lived OpenBao token bound to the `secret-propagator` policy.

### `strategy.matrix.region`

The deployments discovered from the instance tfvars (`llz env list`) — the same
set the scheduled checks verify and the `lke-admin` rotation covers. A deployment
without an OpenBao/cluster yet is skipped internally (`allow-missing` plus the
probe step's pod check).

### Step: `Cluster access (kubeconfig + runner ACL)`

Same preamble as in `rotate`: fetch kubeconfig, open this runner's egress IP in
the control-plane ACL before `kubectl`, revoke at job end (`if: always()`). The
ACL open is guarded on `available` inside the action, because this job tolerates
an unbootstrapped region via `allow-missing`. The step `id` must stay
`kubeconfig` so the `available` gates below keep working.

### Step: `Mint the in-cluster PAT + write secret/linode/api-token`

Skips cleanly when a region is not yet bootstrapped, or when its
`secret-propagator` GitHub-OIDC role is not configured (the operator has not run
`bootstrap-openbao.yml` on this region yet — see
[`docs/runbooks/linode-credential-rotation.md`](../runbooks/linode-credential-rotation.md)).

GHA refetches secrets at the start of each job, so `LINODE_API_TOKEN` here is the
broad token the `create` job just rotated (or the current one in recovery mode).
It is the **minting** credential; the narrow token it mints is what gets written.

## Job: `revoke-linode-pat`

Stateless daily reaper — the label *is* the drain record. Lists every PAT
matching `gha-platform-platform_LINODE_API_TOKEN`, keeps the newest (the live
one), and revokes any sibling older than `grace-days` (default 7).

On the monthly schedule the newly-created PAT is the newest, so the previous
month's PAT — now ~30d old — is eligible for the next day's daily run.

### `if:` guard

Skipped when no deployments exist yet (fresh instance) — `discover` emits `[]`
and `environment: infra-<first deployment>` would be invalid.

### `environment:`

Read-only (it revokes Linode-side PATs and writes no GHA secret) but it still
needs the account token, which is an `infra-<env>` environment secret — read from
the first deployment's environment.

## Job: `create-tf-state-key`

Consumers are GHA-only (the Terraform backend), so no in-cluster propagation is
needed. The Linode OBJ keys API exposes no `created` timestamp, so the companion
drain (`revoke-tf-state-key`) uses a keep-newest-N strategy against the monotonic
`id` field rather than the PAT's grace-by-age approach.

### `if:` guard

Skipped when no deployments exist yet (fresh instance) — `discover` emits `[]`
and `environment: infra-<first deployment>` would be invalid.

### `environment:`

`LINODE_API_TOKEN` + `OPENBAO_SECRETS_WRITE_TOKEN` are `infra-<env>` environment
secrets — read them from the first deployment's environment; `llz` writes the
rotated `TF_STATE_*` pair into *every* deployment's environment via
`gha-secret-deployments`.

### `env.GH_REPO`

`gh secret set` targets the instance repo on github.com (the default host).

### Step: `Rotate OBJ key + write TF_STATE_* to each deployment's env secrets`

`gha-secret-deployments` writes the rotated pair into `TF_STATE_*` for every
deployment's environment.

## Job: `revoke-tf-state-key`

Keeps the 2 most recent same-labeled OBJ keys by `id` (Linode IDs are
monotonically increasing, so highest `id` == newest). With monthly rotation this
gives a ~30-day overlap: the previous key drains the day after the new one is
minted, mirroring the PAT's 7-day-grace overlap.

### `if:` guard

Skipped when no deployments exist yet (fresh instance) — `discover` emits `[]`
and `environment: infra-<first deployment>` would be invalid.

### `environment:`

Read-only drain, but the `LINODE_API_TOKEN` it lists keys with is an
`infra-<env>` environment secret — read it from the first deployment's
environment.
