# Linode Credential Rotation — Runbook

**Applies to:** Linode API tokens / PATs and Linode Object Storage bucket access keys
**Policy:** your org's secret-rotation policy — PAT rotation schedule
**Source of truth:** your Product Security rotation guidelines

This runbook covers the credentials your Product Security rotation guidelines
call out explicitly: **API tokens/PATs (≤90-day expiry)** and **Object Storage
bucket access keys (revoke after 120 days)**. The `lke-admin-token` is a
separate, LKE-Enterprise-specific case — see
[lke-admin-rotation.md](lke-admin-rotation.md).

---

## Credential inventory

| Credential | Type | Policy | Automation | Failure alert |
|------------|------|--------|------------|---------------|
| `LINODE_API_TOKEN` | Linode PAT (broad — LKE/VPC/NB/OBJ) | ≤90-day expiry | Manual create in Cloud Manager; **verified** daily by the `credential-single-pane` scheduled check | Job red |
| `LINODE_DNS_TOKEN` | Linode PAT (DNS scope) | ≤90-day expiry | Same as above | Job red |
| TF-state OBJ key (`TF_STATE_ACCESS_KEY` / `_SECRET_KEY`) | Linode OBJ key | Revoke ≤120 days | **Manual** (bootstrapping paradox — see below) | — (manual SLA) |
| Loki OBJ key (`secret/loki/object-store`) | Linode OBJ key | Revoke ≤120 days | Rotated in-cluster by the `linodeCredRotator` lane (NOT Terraform — the TF-managed keys and their `time_rotating` clock were removed); age verified weekly by the `loki-objkey-rotation-health` step | Job red |
| `OPENBAO_SECRETS_WRITE_TOKEN` | github.com classic PAT | ≤90-day expiry | Per-token expiry measured daily by `llz ci token-inventory` (credential single pane); alerts via `LLZToken*` | Alert fires |
| `APL_VALUES_REPO_TOKEN` | GitHub fine-grained PAT (Contents: write on the instance repo) | ≤90-day expiry | Per-token expiry measured daily by `llz ci token-inventory` (same as the other GitHub PATs). Rotate by minting a new fine-grained PAT and updating the `infra-<env>` env secret(s). Used as apl-core's `otomi.git.password` (apl-operator pushes its values tree) and the argocd repo Secrets. | Job red |

> **Why not a CSPM scanner?** Cloud Security Posture Management tooling
> generally does **not** inspect Linode personal/service-token expiry. The
> `llz ci token-inventory` command (run by the credential-single-pane scheduled
> check) is the concrete substitute.

> **One env list, no drift.** The per-deployment matrices in both
> `secret-rotation.yml` (PAT propagation + lke-admin rotation) and
> `scheduled-checks.yml` (every health/audit job) are derived from a single
> `discover` job that runs `llz env list --json` — i.e. the set of
> `terraform-iac-bootstrap/cluster/<name>.tfvars` files. So rotation writes a
> token into exactly the deployments the daily checks verify, and a new
> deployment (`llz env add`) is covered by both the moment it exists. There is
> no hand-maintained env list to fall out of sync.

---

## 1. API token / PAT expiry (≤90 days)

Linode PATs **cannot** be rotated programmatically with the same scopes — token
creation requires interactive Cloud Manager auth and the scope set is chosen at
creation. So the policy is enforced as **verify-and-alert**, not auto-rotate.

### Automated verification

`instance-template/.github/workflows/scheduled-checks.yml → credential single pane`
runs daily (06:00 UTC, each environment), in two steps sharing one kubeconfig:

1. **`llz ci token-inventory`** (writer) — measures every CI token this job
   holds: GitHub service PATs (via the `GitHub-Authentication-Token-Expiration`
   header) and Linode account PATs (via `GET /v4/profile/tokens`). It writes the
   `llz-token-inventory` ConfigMap, which the in-cluster reconciler re-exposes as
   `llz_token_expiry_*` metrics. Only metadata leaves the job — never a token.
2. **`llz ci alert-eval --strict`** (reader) — asks live Prometheus whether any
   `LLZToken*` / `LLZCertificate*` alert is firing or BROKEN.

A token with no expiry, a lifetime > 90 days, or one already expired surfaces as
a firing alert through Alertmanager rather than only as a red job — the cluster
is the single pane, so the same breach pages the same way whether CI ran or not.

> The former per-provider probe verbs (`llz ci cred-audit`,
> `llz ci gh-pat-expiry`) were retired once this flow subsumed them; their
> measurement lives in `token-inventory` and their reporting in `alert-eval`.

### Rotating `LINODE_API_TOKEN` (automated — `secret-rotation.yml`)

The broad `LINODE_API_TOKEN` is rotated by
`instance-template/.github/workflows/secret-rotation.yml` on the monthly
schedule (1st of the month, 04:00 UTC), or on-demand via `workflow_dispatch`
with `scope=linode-pat`, `pat-apply=true`. The pipeline:

1. **create-linode-pat** — the `linode-pat-rotator` tool mints a new PAT
   (label `gha-<org>-<instance>_LINODE_API_TOKEN`, 90-day expiry) and
   `gh secret set` writes it to the `LINODE_API_TOKEN` env secrets. This broad
   PAT is **CI/Terraform-only** — it is never written into a cluster.
2. **propagate-linode-pat** (matrix: each environment) — `llz ci
   rotate-incluster-pat`: mints that environment's **narrow in-cluster PAT**
   (label `llz-incluster-<region>`; domains/object_storage/volumes rw,
   linodes/vpc ro, firewall rw) using the fresh broad token as the minting
   credential, verifies the new token against the Linode API, writes
   `secret/linode/api-token` in the environment's OpenBao using the
   **`secret-propagator` GitHub-OIDC role** (not root — see below), then
   drains older `llz-incluster-<region>` siblings past a 7-day grace window.
   The narrow token never crosses a job boundary and has no GitHub-secret copy.
3. **revoke-linode-pat** (daily, 03:30 UTC) — `linode-pat-rotator revoke-old`
   drains any same-labeled sibling **broad** PATs older than 7 days.

#### Why GitHub-OIDC, not root

`bootstrap-openbao.yml` revokes the OpenBao root token at the end of every run
by design (the operator is told to delete the env secret too). So
`secrets.OPENBAO_ROOT_TOKEN` is not a live root token outside an active
bootstrap window — propagation can't depend on it.

The **`secret-propagator` GitHub-OIDC (`jwt`) role** (created by
`bootstrap-openbao.yml` → "Configure OpenBao") has a narrow policy: write-only
on `secret/data/linode/api-token`. The `propagate-linode-pat` job presents the
workflow's GitHub OIDC token, gets a short-lived (15m TTL) OpenBao token,
writes, and exits. No long-lived credential is stored on the environment — the
OIDC token is minted per-run by GitHub Actions, so there is nothing to seed,
rotate, or re-seed.

#### Recovery / propagate-only

If the create step succeeds but the per-region job fails (e.g. OpenBao
temporarily unreachable), dispatch `secret-rotation.yml` with
`scope=linode-pat-propagate-only`,
`confirm=rotate:linode-pat-propagate-only`. This skips create and re-runs the
per-region matrix, minting a fresh narrow in-cluster PAT with whatever broad
token is currently in `secrets.LINODE_API_TOKEN`.

#### Regenerating the root token (prerequisite for re-running bootstrap)

`bootstrap-openbao.yml` requires a live root token in
`infra-<env>.OPENBAO_ROOT_TOKEN` and revokes it at the end of the run, so
after any successful bootstrap the env-secret value is stale. The Configure
step preflight (`bao token lookup -self`) will refuse to proceed with a
non-root or revoked token.

`llz openbao regen-root` automates the quorum regenerate-root flow:

```bash
# Point kubectl at the target environment's LKE cluster, then:
llz openbao regen-root <env>
# Three of five unseal-key holders paste their keys when prompted (read in
# terminal raw mode, never echoed or written to disk). Prints the new root token.

# Or seed the env secret directly:
llz openbao regen-root <env> --update-gha-secret [--repo owner/repo]
```

It verifies the new token resolves to a root policy before
exiting. After the next bootstrap-openbao run completes, delete
`OPENBAO_ROOT_TOKEN` from the env secret — the workflow revokes the token
but the env-secret value lingers (and would trip the next bootstrap's
preflight).

### Rotating the narrow-scope PATs (manual — Cloud Manager)

`LINODE_DNS_TOKEN` is not yet wired into `secret-rotation.yml` and remains
manual. (`CLOUD_FIREWALL_TOKEN` was retired: the firewall-controller now reads
the ESO-synced `secret/linode/api-token` — which the rotation pipeline already
keeps fresh — via the cidrFirewall component.)

1. Cloud Manager → **API Tokens** → **Create a Personal Access Token**.
2. Set **Expiry = 90 days** (or shorter). Grant **only** the scopes the old
   token had.
3. Update the matching GitHub **Environment** secret in each `infra-<env>`
   environment (and any repo-scope copy).
4. Re-run a no-op `terraform plan` (terraform.yml) to confirm the new token
   authenticates before the old one is revoked.
5. **Revoke** the old token in Cloud Manager.
6. Confirm the next `credential-single-pane` run is green.

Rotate same-day on operator exit, on InfoSec direction, or on suspected leak.

---

## 2. Object Storage keys — Loki + Harbor registry (revoke ≤120 days)

Linode OBJ keys have **no native expiry** — "revoke after 120 days" means
destroy + recreate. This is now **automated in-cluster** by the llz-reconciler's
`--reconcile-linode-creds` reconciler (the sole rotation mechanism — the
object-storage Terraform module is buckets-only; the TF-managed keys and their
`obj_key_rotation_days`/`time_rotating` clock were removed). **No operator action
in steady state.**

When the current OpenBao secret's `rotated_at` is older than the rotation window,
the reconciler:

1. Mints a **fresh** Linode OBJ key (scoped like the existing one) via the Linode
   API, verifies it works, **then** writes the complete field set straight to
   OpenBao — `secret/loki/object-store` and `secret/harbor/registry-s3` — and
   revokes the oldest, keeping the newest N. Verify-before-write + keep-newest
   means a bad mint or failed write never strands the live credential.
2. ESO syncs the K8s Secret; the consuming pod (Loki / the Harbor registry) reads
   the rotated key on its next start / secret refresh.

**Alerting (a rotation that has fallen behind = the reconciler is down/erroring):**
- In-cluster: `LLZCredentialRotationOverdue` (`llz_credential_age_days > 90`, both
  keys) — the continuous signal.
- Belt-and-suspenders: the `loki-objkey-rotation-health` scheduled check (weekly)
  reads `secret/loki/object-store` age and warns at 105d / fails at 120d.

### If it's overdue

The rotation loop is wedged, not the key. Diagnose the reconciler, not Terraform:

```bash
kubectl -n llz-reconciler logs deploy/llz-reconciler --tail=200 | grep -i linode-creds
```

The log names the failure (Linode API error, OpenBao Kubernetes-auth login, a
non-due `rotated_at`). See [reconciler-alerts.md](reconciler-alerts.md)
(`LLZReconcilerErroring`). Once the underlying fault clears, the reconciler
rotates on its next due-check with no manual reseed.

**Break-glass / suspected leak (rotate NOW, ahead of the clock):** revoke the
leaked key in the Linode Cloud Manager — the reconciler's verify-before-write
then mints + seeds a fresh one on its next pass (or restart the reconciler pod to
trigger a pass immediately). Do **not** reach for Terraform: these keys are no
longer TF-managed.

---

## 3. TF-state Object Storage key (workflow-driven)

`TF_STATE_ACCESS_KEY` / `TF_STATE_SECRET_KEY` is the OBJ key for the Terraform
**state backend itself**. It cannot be rotated *by Terraform* — anything
Terraform could do depends on the state the key guards — but it is **not**
manual: `secret-rotation.yml` rotates it outside Terraform, create-then-revoke.

### Procedure

Dispatch `secret-rotation.yml`:

| Step | `scope` | Arming input | Confirm token |
|---|---|---|---|
| 1. Mint the new pair | `tf-state-key` | `tf-state-apply: true` | `rotate:tf-state-key` |
| 2. Revoke the old pairs, once plans are green | `tf-state-key-revoke` | `tf-state-revoke-apply: true` | `rotate:tf-state-key-revoke` |

Leaving the arming input `false` is a dry run. Step 1 (`create-tf-state-key`)
mints the pair and propagates it to every scope that uses it; step 2
(`revoke-tf-state-key`) drains the old pairs keep-newest-N. Between the two, run
a no-op `terraform plan` and confirm state reads/writes succeed on the new key.

Do **not** hand-swap this credential across Environments — that is the riskiest
possible way to rotate the key guarding Terraform state, and the workflow exists
precisely to avoid it.

---

## 4. GitHub service PATs

The github.com service PATs the repo holds (`OPENBAO_SECRETS_WRITE_TOKEN`) are
**classic** PATs and fall under the ≤90-day rule of your org's secret-rotation
policy. (There is no longer a `GHCR_PAT`: the CI container images are pulled
with the built-in `GITHUB_TOKEN`, so they need no rotatable credential — the
`ci-*` GHCR packages must be public or grant caller repos package-read access.)
The ArgoCD/apl-core values-repo credential is `APL_VALUES_REPO_TOKEN`, a
fine-grained PAT (Contents: write) — it carries an expiration header like the
other GitHub PATs, so the daily credential-single-pane check covers it; rotate
by minting a new PAT and updating the `infra-<env>` env secret(s). (The former
`ARGOCD_REPO_SSH_KEY` SSH deploy key was retired when the in-cluster Gitea was
obsoleted in favour of external HTTPS+PAT git.)

**GitHub provides no API to enumerate classic PATs** (unlike
Linode's `/v4/profile/tokens`). So coverage is a *per-token self-check*, not an
inventory:

### Automated verification

`instance-template/.github/workflows/scheduled-checks.yml → credential single pane`
runs daily. For each known service PAT `llz ci token-inventory` makes one
authenticated request to the matching API (`https://api.github.com`) and
reads the **`GitHub-Authentication-Token-Expiration`** response header. The job
goes red
(the alert) if any token:

- returns **no expiration header** → created with **no expiry** (a
  never-expiring classic PAT — the core policy violation), or
- has an expiry **> 90 days** out (lifetime exceeds policy), or
- returns **401/403** (invalid, revoked, or already expired).

It warns (non-failing) when a token is within 14 days of expiry or its API is
unreachable from the runner.

### What this does NOT cover (residual manual gap)

The self-check only sees the **named service PATs the repo holds as secrets**.
It cannot see **ad-hoc personal/classic PATs created by individuals** against
`github.com` — there is no API for that. That residual surface is an
org audit-log / site-admin review (InfoSec process), not something CI
can automate. Track it as a quarterly manual review with the GitHub org admins.

### Rotating a GitHub PAT

1. Recreate the PAT (`github.com` → Settings → Developer settings → Personal
   access tokens) with **Expiry ≤ 90 days** and the **same scopes** the old one
   had.
2. Update the matching GitHub Actions secret (repo scope — these are not
   environment-scoped).
3. Re-run the consuming workflow (or the scheduled-checks credential single
   pane via `workflow_dispatch`) to confirm the new token authenticates.
4. Revoke the old token.

---

## SLA & alerting

- **Linode PATs:** ≤90-day expiry. Daily `credential-single-pane` (job red on
  breach; warns ≤14 days before expiry).
- **github.com service PATs:** ≤90-day expiry. Measured daily by
  `llz ci token-inventory` (per-token header self-check) and alerted via
  `LLZToken*` on no-expiry / >90d / 401. Ad-hoc individual PATs: manual GitHub
  audit-log review only.
- **Loki OBJ key:** revoke ≤120 days. The `loki-objkey-rotation-health` step of
  the weekly `weekly-cluster-checks` job (warn 105d, job red 120d); replacement
  is the in-cluster `linodeCredRotator` lane, not Terraform.
- **TF-state OBJ key:** revoke ≤120 days. Rotated by `secret-rotation.yml`
  (`scope: tf-state-key` / `tf-state-key-revoke`), with a daily reaper cron
  draining superseded pairs. Linode exposes no OBJ-key creation time, so the
  ≤120-day SLA itself is still calendar-tracked.

These jobs are the alert surface (same as `lke-admin-rotation-health`); there
is no kube-state-metrics secret-age metric
in this Prometheus, so a PrometheusRule would never fire. See
[alerting.md](../alerting.md).

---

## Verification (post-rotation)

1. The relevant scheduled-checks job is green on its next daily run.
2. For OBJ-key rotations: Loki is ingesting/serving logs after the restart
   (`kubectl -n monitoring logs -l app.kubernetes.io/name=loki`).
3. For TF-state-key rotation: a `terraform plan` completes (state read/write OK)
   with the new key and the old key is revoked.
