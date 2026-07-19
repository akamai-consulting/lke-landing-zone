# `llz-terraform.yml` — maintainer rationale

`instance-template/.github/workflows/llz-terraform.yml` is the reusable
(`workflow_call`) Terraform infra pipeline: plan / apply / destroy across the
`cluster`, `vpc` and `object-storage` roots, plus the OpenBao bootstrap and the
converge health gate. It is **vendored** — copier delivers it into every
instance's `.github/workflows/`, where it can never be updated in place. This
document is therefore the home for the maintainer archaeology (incident
post-mortems, PR/issue numbers, "we tried X and it failed because…") that used
to live as comments in the workflow body. The YAML keeps only short comments
covering non-obvious *what* and genuine gotchas.

Sections below are organised by job, and within a job by step name.

---

## Workflow-level

### Locality and the cross-org reuse pattern

The thin caller (`terraform.yml`) invokes this file as
`uses: ./.github/workflows/llz-terraform.yml` with `secrets: inherit`. Same-repo
inheritance actually delivers the instance's repo + environment secrets. This is
the cross-org reuse pattern (`docs/designs/cross-org-reuse-pattern.md`, #201)
that replaced the old `<org>/…@ref` call, whose inheritance arrived **empty** for
any adopter in a different org (#200).

### Step logic stays central

Every job runs in the `llz` container image (`vars.TF_IMAGE`) and calls the
`terraform-init` / `cluster-access` / `fetch-kubeconfig` / `lke-runner-acl`
composite actions from this instance's **own** vendored `.github/actions/`
(referenced locally as `./.github/actions/<name>` — no cross-repo fetch, so the
instance runs self-contained even on an air-gapped GitHub Enterprise). The
single `actions/checkout` per job brings in this instance's
`terraform-iac-bootstrap/` TF roots, `apl-values/`, and the vendored actions;
there is no separate template checkout.

### The `workflow_call` input-forwarding trap

`github.event_name`, `github.ref` and `github.event.pull_request.*` **are**
inherited from the caller, so the push/PR trigger gates work unchanged. The
dispatch **selectors** (`action` / `module` / `region` / `confirm_destroy`) are
**not**: a reusable workflow's `github.event.inputs` is empty across the
`workflow_call` boundary. The caller therefore forwards them as explicit `with:`
inputs, and the jobs gate on the `inputs` context. On push/PR the caller
forwards empty strings, which leaves every action-gated job correctly skipped
(PR plan jobs gate on `github.event_name`, which *does* inherit).

This is the single most load-bearing gotcha in the file and a short version of
it is deliberately kept inline.

The `bootstrap-openbao` job calls the sibling local reusable
`llz-bootstrap-openbao.yml` directly; the PR `discover` job calls the sibling
local `llz-discover-deployments.yml` the same way (vendored, secret-free).

### `secrets:` — why every entry is `required: false`

A green run genuinely needs these secrets, yet all are declared
`required: false`. Instances keep `TF_STATE_*` / `LINODE_API_TOKEN` as
**environment**-scoped secrets (one set per `infra-<deployment>` Environment —
see the env-aware credential rotation). Environment-scoped secrets are **not**
available to satisfy a reusable workflow's `secrets` contract at the call
boundary under `secrets: inherit`; they resolve only at runtime once a job
enters its `environment:`.

A `required: true` secret is validated at *call* time, so it throws a cryptic
`Secret ... is required, but not provided while calling` the moment a nested
`secrets: inherit` `workflow_call` (the OpenBao bootstrap) forces the inherited
bag to be materialised — even though the apply jobs themselves resolve the env
secret fine at runtime. `required: false` defers to runtime; presence is enforced
by the `llz ci require-secret` preflight plus the per-job `environment:`. The
same rationale applies to `APL_VALUES_REPO_TOKEN`.

Refs: `github/docs#44458`, `actions/runner#1490`.

### `inputs.template-ref`

The template release this instance is rendered from; `llz upgrade` re-pins it.
Consumed by the `assert-image-fresh` preflight (cross-checked against the `llz`
baked into `vars.TF_IMAGE` — no template-repo fetch) and forwarded to
`llz-bootstrap-openbao`.

### `concurrency`

`cancel-in-progress: false` — never cancel in-flight infra changes; a partial
apply is worse than a queued one.

### `env:` — the deleted `TF_VERSION`

A `TF_VERSION: '1.9.8'` literal sat in this block, and in 8 other jobs across the
instance workflows. **Nothing read it** — not a step, not a composite action, not
`llz`. Terraform's version comes from the image (`dockerfiles/Dockerfile`'s own
`ARG TF_VERSION`), so this pinned nothing while reading exactly like a pin: a
terraform bump in the image would have left 9 stale literals claiming otherwise.
Tool versions are baked into `vars.TF_IMAGE` and `vars.KUBE_IMAGE`.

---

## Job: `push-noop-notice`

A push to `main` neither plans nor applies — plans run on PRs, applies on
`workflow_dispatch` (what `llz build` / `llz up` fire). So committing a spec
(`llz env add` auto-commits, then you push) produces a run where every *other*
job is correctly skipped, which reads as alarming on a fresh instance. This
always-runs job makes the no-op explicit so the skips are understood as
intentional rather than as a failure.

---

## Job: `discover`

Single source of truth for the PR-plan matrix — see
`llz-discover-deployments.yml`. The same reusable workflow feeds the rotation and
scheduled-check matrices, so the PR plan covers exactly the deployments those do.

---

## Job: `plan-cluster-pr`

Runs for every PR that touches a Terraform path. Posts a plan summary; never
applies. Uses read-only remote state credentials.

**Security.** Plan jobs consume production secrets (`LINODE_API_TOKEN`,
`TF_STATE_*`). They are restricted to internal PRs only —
`github.event.pull_request.head.repo.full_name == github.repository` — so fork
PRs are skipped, preventing untrusted code from reading secrets via the runner
environment. See GitHub's "Keeping your GitHub Actions and workflows secure /
Preventing pwn requests".

### Step: `Terraform init — cluster`

The per-env tfvars are gitignored build artifacts rendered from the spec; the
`terraform-init` composite regenerates them before `init`, so no separate render
step is needed here (the first TF step after init is the plan). No-op for
instances without a spec.

---

## Job: `tf-lint`

Static analysis of Terraform HCL using rules in `.tflintrc.hcl`. Needs neither
provider credentials nor remote state — runs against local HCL only.

---

## Job: `checkov`

Checks both Terraform modules for security misconfigurations: public bucket
exposure, missing encryption, overly permissive IAM, and provider-specific best
practices. Runs against local HCL; no credentials required.

### The retired SARIF upload step

An "Upload Checkov SARIF report" step ran after the scan, uploading
`results_*.sarif`. The `make` target never passed `-o sarif` or an output path,
so that file was never produced; `if-no-files-found: ignore` kept the step
permanently green, and nothing downloaded the artifact or fed it to
`codeql-action/upload-sarif`. It read as security coverage and was none. The scan
itself still **gates** — the make target exits non-zero on a finding, which is
what actually enforces. Re-add the upload only together with
`-o sarif --output-file-path` and a real consumer.

The YAML keeps a two-line pointer at the removed step.

---

## Job: `promote-pipeline-drift`

`promotion_rank` (`cluster/<env>.tfvars`) is the source of truth for the native
promotion pipeline; `.github/workflows/promote.yml` is **rendered** from it by
`llz env add` / `llz env pipeline` (`docs/environments-and-promotion.md` §4).
This gate fails a PR that edits a rank without regenerating `promote.yml` — the
"did you regenerate?" check. It is a no-op for instances with fewer than two
ranked deployments (the chain only forms at two stages). Regenerate locally with
`llz env pipeline`. `llz` is baked into `vars.TF_IMAGE`, the same as the plan
jobs' `llz ci …`.

---

## Job: `apply-vpc`

### Apply/destroy dispatch model

Apply is handled by `release.yml` on every GitHub release (all regions). The
`workflow_dispatch` path here exists for emergency infrastructure-only changes
that don't warrant cutting a release (e.g. rotating credentials, updating ACLs).
The `infra-primary` and `infra-secondary` GitHub Environments each have required
reviewers; approval is logged with reviewer identity and timestamp in the GitHub
Deployments API audit trail.

### Shared VPC (`spec.networks`)

Spec-driven instances may put several **same-region** deployments in one VPC
(`cluster.network.vpc`). That VPC is cross-deployment state (key `vpc/<network>`),
so it is created here, once, before the cluster that attaches to it — the cluster
root then looks it up by label. A deployment using a dedicated VPC (the default)
resolves no network and this job is a no-op.

Linode OBJ has no state locking, so a single concurrency group
(`llz-shared-vpc-apply`) serializes all VPC applies to avoid concurrent
same-state corruption. The promotion chain already orders pipeline stages; this
guards ad-hoc concurrent builds.

### Front-loaded preflights

`apply-vpc` is the first job `apply-cluster` depends on, so failing here aborts
**before** the ~15-minute cluster apply. Two cheap fail-fast checks live here:

1. **Image/template skew** (`Pre-flight — ci-terraform image matches
   template-ref`). The instance pins `TF_IMAGE` (baked `llz`) and `template-ref`
   independently. When the image lags, the checked-out workflow calls `llz`
   commands/flags the baked binary lacks, surfacing far downstream as a cryptic
   "unknown flag" or a silently no-op'd gate. `llz ci assert-image-fresh` asserts
   they match.

2. **Env secrets** (`Pre-flight — verify infra-<region> env secrets`) that the
   cluster-bootstrap apply needs: `APL_VALUES_REPO_TOKEN` and
   `OPENBAO_SECRETS_WRITE_TOKEN`. Validated here rather than in
   cluster-bootstrap (which runs *after* the 15-minute cluster apply) — a missing
   token now fails in seconds instead of wasting the cluster apply.
   cluster-bootstrap keeps its own check as defence in depth.

   - `APL_VALUES_REPO_TOKEN`: fine-grained GitHub PAT (Contents: write) for the
     instance repo. apl-core's `otomi.git.password` and the argocd repo Secrets
     render empty without it; apl-operator can't push and Argo can't clone.
   - `OPENBAO_SECRETS_WRITE_TOKEN`: fine-grained PAT (github.com) scoped to this
     repo with Actions + Secrets: write — used by `bootstrap-openbao`
     (GitHub-token seeds and recovery-key/root-token persistence) and by the
     destroy-time GH-secret cleanup.

### Step: `Resolve shared VPC for this deployment`

Which shared VPC (if any) this deployment attaches to is resolved from the spec
(`llz env vpc`) — no rendered tfvars are needed here. The shared VPC's
`vpc/<network>.tfvars` is regenerated by the `terraform-init` composite before the
plan reads it. An empty result means a dedicated VPC, created by the cluster
apply itself — nothing to do here.

---

## Job: `apply-cluster`

### `LLZ_PHASE_LOG`

Phase-timeline log (`docs/designs/e2e-instrumentation.md`): captures the precise
LKE-E create wall-time — the baseline the HA-control-plane-off experiment
compares against — cleaner than the job-level number.

### Step: `Render tfvars from the LandingZone spec (spec-driven instances)`

The per-env tfvars are gitignored build artifacts. They are rendered
**explicitly** here because the capacity-guard preflight below reads
`<region>.tfvars` (`cluster_label` / `node_type` / `node_count`) and runs
**before** the `terraform-init` composite that would otherwise render them. No-op
for instances without a spec.

### Step: `Pre-flight — Linode account capacity / orphan check`

Catches account-quota exhaustion **before** the apply, rather than via a
30-minute cluster-create hang, PVCs Pending on `[400]`, or a provider
`ReadResource` hang. Orphaned Volumes, CCM NodeBalancers and LKE VPCs left by a
failed or partial destroy accrete across rebuild cycles and eat the account's
active-services limit.

Scoping rules:

- NodeBalancers and VPCs are scanned **account-wide** — they carry a cluster-id
  tag/label, so a gone-cluster orphan is unambiguous in any region.
- `pvc-*` Volumes are scoped to **this deployment's region** via
  `--volume-region`. A detached Volume carries no cluster id, so an account-wide
  count would flag other regions'/teams' detached Volumes that `llz reap`
  (region-scoped; it refuses unscoped Volume sweeps) will never clean — the gate
  would then disagree with reap and block the apply on Volumes no one can
  attribute. Block-storage quota is per-region anyway, so only same-region
  Volumes compete with this apply.

Tunable via repo vars: `PREFLIGHT_ORPHAN_THRESHOLD` (default 5), or
`PREFLIGHT_FAIL_ON_ORPHANS=false` for report-only.

### Step: `Terraform apply — cluster`

Wrapped (`llz ci tf-apply`) so a duplicate Cloud-Firewall-label create — an
orphan from a prior cancelled run, or one the import-before-plan step missed —
self-heals: the wrapper imports the existing firewall by label and retries,
instead of failing the whole provision on a `[400]` label collision.

### Step: `Report cluster-create timing` / `Upload create timing artifact`

Precise LKE-E create wall-time to the step summary plus an artifact (the HA-off
baseline). Best-effort (`continue-on-error`); instrumentation never affects the
apply outcome. See `docs/designs/e2e-instrumentation.md`.

---

## Job: `bootstrap-openbao`

### Why the standalone cluster-bootstrap job is gone

The in-cluster bootstrap (apl-core + Argo bridge) used to be its own job here. It
paid its own container-init + checkout + cluster-access/ACL cycle (~1.5–2 minutes
of pure overhead) immediately before `bootstrap-openbao` did the same on the same
cluster. The cluster phase (`wait-cluster-ready` + `llz ci bootstrap-cluster`) now
runs at the head of `llz-bootstrap-openbao.yml`'s `bootstrap` job, sharing **one**
cycle — the same fold that absorbed the former standalone `converge` job at that
job's tail.

### The chained bootstrap path

Tier-1 streamlining: the operator used to trigger two workflows manually —
`terraform.yml` first, then `bootstrap-openbao.yml` within a 60-minute window.
With `bootstrap-openbao.yml`'s `workflow_call:` trigger it is chained here, so a
single `module=all` apply walks the whole bootstrap path end to end:

1. `apply-object-storage` — S3 buckets only; the scoped keys are minted later by
   `bootstrap-openbao`'s `mint-bootstrap-objkeys`, no GitHub stash.
2. `apply-cluster` — Linode LKE-E cluster.
3. `bootstrap-openbao` with `bootstrap_cluster: true` — ONE job runs the
   in-cluster bootstrap (`wait-cluster-ready` + `llz ci bootstrap-cluster`:
   apl-core + foundation + Kyverno policy), then the OpenBao init/unseal/seed +
   ESO sync, then the final `llz ci converge` health gate. The former standalone
   `bootstrap-cluster` and `converge` jobs are folded into it so the chain pays
   one container-init + checkout + cluster-access/ACL cycle instead of three.

Step 3 also settles the PreSync timeout-window concern — by running immediately
as part of the same workflow, there is no chance the operator forgets or runs out
of the 60-minute budget the gate enforces.

Operators can still trigger `bootstrap-openbao.yml` on its own via
`workflow_dispatch` (the dispatch trigger is preserved, `bootstrap_cluster`
defaults to false there) for retries and post-seed configuration changes.

### The `if:` expression

The condition mixes a default-skip-on-`needs`-failure guard with explicit
`needs.*.result` checks, so `apply-cluster` is required to have **succeeded**
while `apply-object-storage` is accepted as either success **or** skipped.

`apply-object-storage` now runs on `module=cluster` too (its buckets are always
needed), so on a normal cluster bootstrap it succeeds and the buckets exist
before this job's `mint-bootstrap-objkeys` mints their scoped keys. The
`|| skipped` arm is kept as a backstop for forward-compat (e.g. a future `module`
value that legitimately skips object-storage); a **failed** object-storage apply
still correctly blocks this job. Without `always()` the default `success()` would
skip this job whenever `apply-object-storage` is skipped.

**apply only — not plan.** The `bootstrap_cluster` phase *consumes* a live cluster
(reads the live coredns Service, helm-installs, applies manifests), so a
plan-mode dispatch (the release-e2e dry run) that creates no cluster has nothing
to bootstrap. `action=plan module=all` cleanly plans the cluster-creating roots
and stops there. PR previews use `plan-cluster-pr`.

### `needs:`

- `apply-cluster` — the LKE-E cluster exists; the merged bootstrap job's
  `bootstrap_cluster` phase brings up apl-core itself (the former standalone
  `bootstrap-cluster` job is folded into it).
- `apply-object-storage` — the S3 buckets exist. `bootstrap-openbao`'s preflight
  asserts the object-storage state is populated before `mint-bootstrap-objkeys`
  mints the scoped keys against them. It now runs on `module=cluster` too; the
  `|| skipped` arm in the `if:` above is a forward-compat backstop.

### `secrets: inherit`

Forwards **all** repository and environment-scoped secrets from the caller.
`bootstrap-openbao.yml`'s job declares `environment: infra-${{ inputs.region }}`
so env-scoped secrets resolve correctly at job-evaluation time on the runner.

---

## Destroy path (overview)

Explicitly gated: `action` must equal `destroy`. Completely separate from the
apply job path so an apply run cannot accidentally destroy.

Order:

1. `pre-destroy-cluster` — in-cluster finalizer unwedge + GH-secret cleanup,
   while the cluster is still reachable.
2. `plan-destroy-cluster` — Linode plan, informational.
3. `destroy-cluster` — delete Linode resources; reaps every in-cluster object
   with the cluster.

There is no longer a cluster-bootstrap Terraform workspace to destroy: the
in-cluster resources are ArgoCD/apl-core-owned and disappear with the LKE
cluster, so no `terraform destroy` / `state rm` untrack is needed. All that must
happen before the cluster delete is the best-effort finalizer unwedge plus the
cluster-scoped GH env-secret cleanup — a slim, TF-free pre-destroy job.

---

## Job: `pre-destroy-cluster`

### Step: `Checkout the instance`

Checkout runs **before** the destroy-confirm gate: `llz` runs from the container
image, and the checkout brings in the `cluster/<region>.tfvars` label source plus
the confirm token.

### Step: `Render tfvars from the spec`

So `destroy-unwedge` can resolve the cluster label. (Composite actions are
vendored at `./.github/actions`; the former cluster-bootstrap TF destroy jobs are
gone with the workspace.)

### Step: `Open control-plane ACL for this runner`

Best-effort cleanups need control-plane access, so this opens the runner's egress
IP in the ACL. `fail-on-missing: 'false'` makes an already-gone cluster a clean
no-op. Revoked at job end via `if: always()`.

### Step: `Unwedge namespace finalizers before destroy`

Clears the Argo/discovery/CNPG finalizer deadlocks while the cluster is still
reachable (formerly `null_resource.unwedge_namespace_finalizers_on_destroy`).
`--region` resolves the cluster kubeconfig by its `cluster/<region>.tfvars` label
via the Linode API. Best-effort — `continue-on-error` so an already-gone cluster
or any resolution failure never blocks the teardown; the downstream DESTROY
Cluster job reaps every in-cluster resource regardless.

### Step: `Clear cluster-scoped OpenBao/Harbor env secrets`

Deletes the cluster-scoped OpenBao/Harbor GH env secrets (formerly
`null_resource.clear_openbao_secrets_on_destroy`) so a destroyed cluster's stale
secrets — revoked root token, unseal keys, Harbor creds — can't poison the next
bootstrap. Best-effort per secret; an unset `GH_TOKEN` is a no-op.

---

## Job: `plan-destroy-cluster`

Checkout runs before the destroy-confirm gate for the same reason as
`pre-destroy-cluster`: `llz` runs from the container image, and the checkout
brings in the TF roots plus the confirm token.

---

## Job: `destroy-cluster`

### `needs:`

`pre-destroy-cluster` (finalizer unwedge + GH-secret cleanup) is best-effort and
allowed to fail — cluster unreachable, etc. The cluster destroy still proceeds
because the K8s objects disappear with the cluster. Hence `always()` in the `if:`.

### Step: `Capture LKE cluster id + pvc Volume ids (for the sweeps)`

Captures cluster-scoped identifiers **before** the destroy deletes them, for the
straggler sweeps below. Both sweeps must be scoped to **this cluster**, never to
the region: co-located deployments (primary/staging/lab all → `us-ord`) share a
Linode region *and* the block-storage Volume tag, so a region-wide filter both
fails to converge (a live peer's resources keep the count above zero forever) and
risks deleting a peer's resource.

- NodeBalancers are scoped by the cluster's CCM tag (`lke<id>`), so only the id
  is needed.
- Volumes carry no cluster id, and once detached lose their `linode_id` — so the
  step must snapshot, by id, exactly the `pvc-*` Volumes attached to this
  cluster's nodes while the cluster still exists.

### Step: `Force-delete remaining Linode resources via API`

Handles the case where the cluster was in an indeterminate state and Terraform
couldn't reach or import it: deletes the cluster by label, then the node firewall
— exact `node_firewall_id` output first, then the module-correct label. Never a
reconstructed `"<cluster>-nodes"` guess, which ignored `var.firewall_label` and
leaked the firewall every teardown.

The VPC is deliberately **not** deleted here; it gets a dedicated step at the END
of this job, after the Volume and NodeBalancer sweeps — a CCM NodeBalancer parked
in the VPC subnet blocks the VPC delete with a 409 until then.

**Gate rationale (shared by every sweep step below).** `always()` so this runs
even if `terraform destroy` failed mid-way, but gated on the confirmation having
**passed** — a failed confirm means nothing was torn down, so there is nothing to
force-delete or sweep, and the Volume sweep would otherwise poll its full 10
minutes waiting for volumes to detach from a cluster that is still alive.

### Step: `Sweep orphaned Block Storage Volumes` (authoritative)

The `block-storage-retain` StorageClass is `reclaimPolicy: Retain`, so CSI
**never** deletes its Block Storage Volumes — only an API sweep does. This is the
authoritative sweep: it runs **after** the cluster delete, where the `pvc-*`
Volumes are finally detached (`linode_id == null`), and it is what keeps Retain
Volumes from accreting toward the account quota across rebuild cycles. An
in-cluster sweep before the cluster delete would find every Volume still attached
and delete nothing.

Scoped to the `pvc-*` Volume ids snapshotted before the destroy (the Volumes
attached to *this* cluster's nodes). If none were captured, the cluster was
already gone — any attached `pvc-*` Volumes in the region belong to a live
co-located peer, so there is nothing to do.

Cluster deletion detaches these Volumes asynchronously as the LKE Linodes tear
down. `--wait-detach` polls the **tracked** ids (≤10m), so a live co-located
peer's Volumes can't stall it — the region-wide poll's old failure mode — and can
never be deleted. `--require-empty` then re-lists and, if any tracked Volume
survives the sweep (one that never detached, or a delete still settling), retries
up to `--attempts` and finally **fails** the job when orphans remain. A leaked
Volume would otherwise sail past this green-checkmarked destroy and stall the
next apply's preflight on the account quota.

### Step: `Sweep orphaned NodeBalancers` (authoritative)

NodeBalancers are created by the in-cluster CCM for every LoadBalancer Service
(Istio ingress, etc.). Deleting the LKE cluster normally reaps them — but because
this destroy path deliberately does **not** gracefully delete the Services first
(the apl-core chain is dropped from state and the cluster is nuked), the CCM never
issues its own NodeBalancer DELETE. Reaping is left entirely to LKE
cluster-delete, which has been seen to leak the occasional NodeBalancer (ongoing
billing plus a held IPv4).

This sweep deletes any survivor still carrying the cluster's CCM tag (`lke<id>`).
It runs after the Volume sweep above, which polls up to 10 minutes — by then the
cluster delete has long completed and any reap-able NodeBalancer is already gone,
so this only catches genuine stragglers.

`--require-empty` re-lists after the sweep and, if any NodeBalancer still carries
this cluster's CCM tag, retries up to `--attempts` and finally fails the job — a
leaked NodeBalancer keeps billing plus an IPv4 and counts against the next
apply's preflight quota.

If `LKE_CLUSTER_ID` was never captured (the cluster may already have been gone),
the step warns and skips rather than performing an unscoped delete. Finish
manually once the id is known:
`LINODE_TOKEN=<token> llz --yes ci reap-nodebalancers --cluster-id <id>`.

### Step: `Delete cluster VPC` (LAST — after the NB + Volume sweeps)

A VPC can only be deleted once **nothing** is parked in its subnet. CCM
NodeBalancers (swept in the step above) and attached node Linodes both block it
with a 409, so the VPC delete **must** come after those sweeps. Doing it in the
force-delete step, before the sweeps, guaranteed a 409 and leaked the VPC every
teardown.

Resolution order: the exact `vpc_id` output first, then the
`"<cluster_label>-vpc"` label, then — for the LKE-E auto VPC labeled `lke<id>`,
which no output or BYO label names — the captured cluster id. `llz` reads that
from the `LKE_CLUSTER_ID` env var `teardown-capture` exported, so **no new flag**
is passed here: a stale baked `llz` (the ci-terraform image lags a `tools/` change
until rebuilt) just ignores the env instead of dying on an "unknown flag". Retry
rides out the async-release window.

### Step: `Reap this deployment's minted obj-keys + in-cluster PAT`

The loki + harbor-registry Object Storage keys (`platform-loki-<env>` /
`platform-harbor-registry-<env>`) and the narrow in-cluster PAT
(`llz-incluster-<env>`) carry no cluster tag, so the Volume/NB/VPC sweeps above —
and `llz reap`'s cluster-liveness pass — never touch them. Each
bootstrap/rotation mints fresh ones under a stable per-env label. Without this,
a destroyed env's keys/PAT leak and accrete toward the account's 100-key /
100-PAT caps until a future mint returns `400 ("reached your access key quota")`
and bootstrap dies at `mint-bootstrap-objkeys`.

Exact-label match on **this env only**; never the broad token the job runs under.
Best-effort — a leftover key is a quota nuisance, not a reason to fail a clean
destroy.

### Step: `Assert no orphaned resources remain` (final gate)

The scoped Volume + NodeBalancer sweeps above only fire when the cluster still
existed at `teardown-capture` time. When it was **already gone** — a re-run of a
partial destroy, or pre-existing orphans from an earlier rebuild cycle — those
sweeps no-op. Without this gate the destroy goes green while leaving orphans that
then stall the next apply's preflight. Observed in practice: a "successful"
destroy left 5–7 orphaned `pvc-*` Volumes / NodeBalancers / VPCs and the next
apply failed preflight.

This step re-counts orphans account-wide with the **same** census the preflight
uses, retries to ride out the cluster-delete's async NB/VPC reap and Volume
detach, and **fails** the destroy if any remain. It deletes nothing — clear
survivors with `llz reap`. It is account-wide and uses the same threshold as the
apply preflight, so a destroy that leaves the account in a state the next apply
would reject fails loudly here instead.

`llz` also reads `LKE_CLUSTER_ID` from the env (exported by `teardown-capture`),
so **this** cluster's own survivors (NodeBalancers by `lke_cluster.id`, VPC
labeled `lke<id>`) fail the gate regardless of `--threshold`. The threshold only
tolerates other-account orphans the destroy can't clean, and must never mask the
cluster just destroyed. No new flag here, for the same stale-baked-`llz` reason as
the VPC step.

---

## Job: `apply-object-storage`

Creates the Loki + Harbor-registry S3 **buckets** for each region. Buckets only:
the scoped access keys are no longer TF-managed — `bootstrap-openbao`'s
`llz ci mint-bootstrap-objkeys` mints and seeds them, and the in-cluster
`linodeCredRotator` rotates them. No `LOKI_S3_*` / `HARBOR_REGISTRY_S3_*` GitHub
secrets, no stash step.

Required `infra-<region>` environment secrets beyond the standard set:
`LINODE_API_TOKEN` (Linode API token for the Object Storage provider).

### Why it runs on `module=cluster` too

Not just `object-storage`/`all`. Loki and Harbor are **always** installed and
both require their S3 buckets + scoped keys to exist, so object-storage must be
provisioned on every cluster bootstrap. Leaving it opt-in let cluster-only
rebuilds silently skip it, so Loki persisted zero chunks and Harbor's registry
couldn't write to S3 — with no signal. Observed in practice.

The apply is idempotent (0 changes once the buckets/keys exist) and lives in its
**own** TF state, so making it always-run does **not** couple bucket lifecycle to
the cluster: a cluster destroy still never touches the buckets (destroy is
module-scoped via `destroy:<region>:object-storage`), preserving log/image
durability across rebuilds. `bootstrap-openbao` already `needs:` this job and
mints the scoped keys against the buckets it creates.

### The retired credential-stash step

A "Store object-storage credentials as environment secrets" step was retired
along with the TF-minted keys. `llz ci mint-bootstrap-objkeys` in
`bootstrap-openbao.yml` mints the scoped keys and seeds OpenBao directly — no
`LOKI_S3_*` / `HARBOR_REGISTRY_S3_*` GitHub secrets, no reseed relay.

---

## Job: `plan-destroy-object-storage`

Checkout runs before the destroy-confirm gate (`llz` runs from the container
image; the checkout brings in the TF roots plus the confirm token).

---

## Job: `destroy-object-storage`

Checkout runs before the destroy-confirm gate, same as above.

### Step: `Drain S3 buckets before destroy`

Buckets must be drained before the TF destroy. The Linode provider does **not**
support `force_destroy=true` on `linode_object_storage_bucket`, so a bucket with
any remaining objects (Loki chunks, Harbor registry layers, gitea dumps) fails
the destroy with "bucket is not empty". This step bulk-deletes each bucket's
contents so TF destroy proceeds against empty buckets.

**Why s5cmd, not awscli.** `TF_IMAGE` (debian-slim + terraform/kubectl/helm/
kustomize) ships **neither** awscli **nor** python+pip — the previous
awscli-via-pip approach hit `pip3: command not found` on the first real-world
run. s5cmd is a single static Go binary (`peak/s5cmd`) downloaded into `/tmp` on
each invocation (~5 MB tarball), so no `apt-get` or user-permission gymnastics.

**Supply-chain pinning.** The version is pinned **and** the tarball sha256 is
verified before extracting and executing, since this binary handles the
loki/harbor/gitea access keys in env. If GitHub's CDN ever serves a different
tarball (compromise, typosquat, TLS bypass), the `sha256sum` check fails and the
step exits non-zero before `chmod` + execute. When bumping `S5CMD_VER`, refresh
`S5CMD_TAR_SHA256` with `curl -fsSL <url> | sha256sum`.

`curl --retry` rides out transient GitHub release-CDN blips (429/503/timeouts) so
a one-off hiccup can't fail teardown and leave paid S3 buckets undrained. It is
deliberately **not** `--retry-all-errors` (a real 404 should fail fast); the
sha256 check still guards integrity.

**Tolerant by design.** Failure to drain any single bucket emits a `::warning::`
but doesn't fail the step — TF destroy then surfaces the specific bucket that's
still non-empty, and the operator falls back to manual cleanup. The state of any
already-destroyed key/bucket is handled by the guard on each `terraform output`
read; outputs disappear from state as their backing resources are destroyed in a
retried run.

**Why `terraform output -json`, once.** The previous `terraform output -raw
<name>` approach leaked TF's "Warning: No outputs found" text into the captured
value when state had zero outputs (observed on a re-run after a partial destroy).
That text got embedded into bucket names and endpoint URLs, breaking s5cmd with
`bad value for --endpoint-url`. `-json` returns a clean `{}` on no-outputs and a
clean `{name: {value, type, sensitive}}` otherwise, with no inline warnings.
`2>/dev/null` keeps any stderr quiet either way.

**Credentials.** A **temporary** scoped key is minted around this sweep — the
Loki/Harbor keys are no longer TF-managed (see the module's "Access keys" note),
so there are no key outputs to read. `llz ci temp-objkey create` exports
`TEMP_OBJKEY_{ID,ACCESS,SECRET}` to `$GITHUB_ENV` for the `always()` delete step;
this same shell reads the creds back from that file, because env-file writes only
apply to **later** steps.

**s5cmd invocation details.** s5cmd reads `AWS_*` env vars and uses
`--endpoint-url` for the custom endpoint. `AWS_REGION` must be non-empty for
sigv4 signing even though Linode OBJ ignores the value. `s3://bucket/*` matches
all objects recursively in s5cmd's wildcard semantics. Errors when the bucket is
already empty are tolerated by the `||` guard.

The gitea-backup bucket drain was removed alongside the bucket/key/outputs in the
apl-core anti-pattern cleanup.

### Step: `Delete temporary drain key`

Revokes the temporary drain key regardless of drain outcome. No-op when the
create was skipped (already-drained re-run, mint failure).
