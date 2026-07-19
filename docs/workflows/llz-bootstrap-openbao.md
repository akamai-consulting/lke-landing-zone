# `llz-bootstrap-openbao.yml` — maintainer notes

Reusable (`workflow_call`) workflow that initializes, unseals, and configures an
OpenBao cluster for a given deployment — and, when `bootstrap_cluster=true`, runs
the whole in-cluster bring-up (apl-core, Kyverno policies, the Argo bridge) and the
final convergence gate in the same job. It is **vendored**: copier delivers
`instance-template/.github/workflows/llz-bootstrap-openbao.yml` verbatim into every
instance's `.github/workflows/`, so the file that ships to customers cannot be
updated in place. This document is the archive for the maintainer rationale —
incident post-mortems, e2e run IDs, rejected designs, and the history behind the
current shape. The YAML keeps only what a person debugging a live run at 3am needs.

Related: `docs/architecture/convergence-contract.md`,
`docs/designs/cross-org-reuse-pattern.md` (#201), `docs/designs/e2e-instrumentation.md`.

---

## Workflow-level

### Why it is LOCAL to the instance

Per `docs/designs/cross-org-reuse-pattern.md` (#201), copier delivers this file into
each instance's `.github/workflows/`. Both the standalone `bootstrap-openbao.yml`
caller and `llz-terraform.yml`'s `bootstrap-openbao` job invoke it as
`uses: ./.github/workflows/llz-bootstrap-openbao.yml` + `secrets: inherit` —
same-repo, so the instance's environment secrets actually resolve. The body runs in
the `llz` container image and calls the `cluster-access` / `fetch-kubeconfig` /
`lke-runner-acl` composite actions from the instance's own vendored
`.github/actions/` tree (`./.github/actions/<name>`); the single instance checkout
brings in `terraform-iac-bootstrap/cluster` (fetch-kubeconfig's working-dir) AND
those actions. `inputs.*` are `workflow_call` inputs forwarded by the caller.

### Convergence contract

The first-init / wait-for-unseal / re-configure mode selection is the same
detect → choose a path → re-verify shape the cluster-health contract uses, applied to
OpenBao seal state — and it lives in ONE place: the `llz ci bao-ensure-ready` command
(`tools/cmd/llz/ci_bao_ensure_ready.go`), run as a single step. Under the chart's
`seal "static"` auto-unseal each pod unseals itself at boot from the static seal key
(created by the `bao-seed-seal-key` step before the pods start), so there is no
submit-keys flow and no scheduled re-unseal cron — `bao-ensure-ready` just runs
`bao operator init` on a fresh cluster and waits for the pods to converge to unsealed.

Likewise the generic KV seeds run from one data-driven step (`llz ci bao-seed-all`,
table in `ci_bao_seed_all.go`) rather than one inline `bao-seed` step per secret.

**When adding new steps:** classify failures into the 3 contract codes (poll vs
hard-fail vs converged) rather than dropping a 13th `BOOTSTRAP_ERRORS=true` soft-fail
flag. The aggregated-soft-fail pattern is the one we're explicitly trying to remove.

### The three modes

**First-time bootstrap (uninitialized cluster)**

* Seeds the 32-byte static auto-unseal key Secret so the pods can start + auto-unseal
  (`bao-seed-seal-key`; key backed up to `infra-<region>`)
* Runs `bao operator init` → 5 RECOVERY keys + root token (recovery keys authorize
  `generate-root`; they do NOT unseal — the static seal key does)
* Stores recovery keys 1–3 as `infra-<region>` environment secrets
* Prints all 5 recovery keys + root token to the job summary (copy offline)
* Waits for all 3 pods to auto-unseal (followers join via `retry_join`)
* Configures KV v2, Kubernetes auth, GitHub-OIDC auth, policies, roles, audit log
* Seeds `secret/infra/github-dispatch-token` and the other env-sourced KV paths
  (`secret/harbor/admin`, `grafana/admin`, `otel/ingress` now arrive via ESO PushSecrets)
* Revokes the root token on completion

**Re-seal recovery (initialized but sealed, e.g. after pod restart)**

* Pods self-unseal from the static seal key; this path just waits for them
* Skips all configuration steps

**Re-configure (initialized + unsealed, configuration incomplete)**

* Set `OPENBAO_ROOT_TOKEN` as an `infra-<region>` environment secret
* The workflow detects it and runs configuration + seed steps
* Delete the `OPENBAO_ROOT_TOKEN` secret immediately after the run

### Required `infra-<region>` environment secrets

| Secret | Purpose |
| --- | --- |
| `LINODE_API_TOKEN` | Linode API token |
| `TF_STATE_ACCESS_KEY` | S3 backend access key |
| `TF_STATE_SECRET_KEY` | S3 backend secret key |
| `OPENBAO_SEAL_KEY` | Set automatically on first bootstrap; the 32-byte static auto-unseal key (restored into the cluster on a rebuild) |
| `OPENBAO_RECOVERY_KEY_1/2/3` | Set automatically on first bootstrap; authorize `generate-root` |
| `OPENBAO_ROOT_TOKEN` | Optional; set manually to trigger re-configuration |
| `OPENBAO_SECRETS_WRITE_TOKEN` | Fine-grained PAT (github.com), this repo, Actions + Secrets: write; used by `gh secret set` for seal/recovery/Harbor credentials and seeds the harbor-ready dispatch token |

Required repository variables: `TF_STATE_BUCKET` (S3 bucket for Terraform state),
`TF_STATE_ENDPOINT` (S3-compatible endpoint URL).

### Why every declared secret is `required: false`

A green bootstrap genuinely needs several of these, yet all are declared
`required: false`. This workflow is reached via a nested `workflow_call` under
`secrets: inherit` (`llz-terraform.yml` → here). `secrets: inherit` forwards only
repository/org secrets across the call boundary — environment-scoped secrets (the
instance keeps `TF_STATE_*` in its `infra-<region>` Environment) are NOT inheritable,
and a `required: true` declaration is validated statically at the call boundary
BEFORE any job enters `environment: infra-<region>`. With `required: true` that
yields a cryptic call-time "required, but not provided" failure on the very first job
(Resolve HA role), which doesn't even use these secrets. The `bootstrap` job declares
`environment: infra-<region>` and resolves them at runtime; `llz ci require-secret`
preflights presence with a friendly error. Same rationale as `APL_VALUES_REPO_TOKEN`
in `llz-terraform.yml`.

`APL_VALUES_REPO_TOKEN`, `LINODE_DNS_TOKEN`, `GHCR_READ_TOKEN` and
`APPS_REPO_REVISION` are consumed only by the `bootstrap_cluster=true` phase (the
merged former `bootstrap-cluster` job): apl-core's `otomi.git.password`, the optional
DNS token, and the optional private-GHCR read credential.

`LOKI_S3_*` / `HARBOR_REGISTRY_S3_*` were **retired**: the object-storage keys are
minted by `llz ci mint-bootstrap-objkeys` and rotated in-cluster — the credentials
never transit GitHub.

### Inputs worth explaining

* **`template-ref`** — the template release this instance is rendered from;
  `llz upgrade` re-pins it. Unused by this workflow's jobs (everything resolves
  locally); declared only because the caller stub passes it and `workflow_call`
  rejects undeclared inputs.
* **`assert_loki`** — default false for normal instances. The release-e2e gate sets it
  true so the check runs inside the converge that already holds cluster access,
  instead of a separate `validate` pass dispatching `cluster-health.yml`.
* **`bootstrap_cluster`** — set by `llz-terraform.yml`'s chained call so cluster +
  OpenBao bootstrap share ONE container-init + checkout + cluster-access/ACL cycle.
  The former standalone `bootstrap-cluster` job paid its own, ~1.5–2m of pure overhead
  on the e2e critical path. Default false: a standalone `bootstrap-openbao` dispatch
  (retries, re-configuration) assumes the cluster is already bootstrapped.
* **`preserve_root_on_failure`** — DEBUG ONLY. Skips the post-bootstrap root-token
  revoke when bootstrap FAILS so an operator can iterate. Default false.

---

## Job: `resolve` — Resolve HA role

`role` (active|standby|standalone) and `peer` (the other member of its `ha_group`) are
read from the cluster tfvars via `llz env` (baked into `vars.TF_IMAGE`) and drive: the
Harbor seed direction (a standby seeds from its active peer), the
`github-dispatch-token` standby skip, and the cross-region CA provisioning (a standby
ships its CA to `infra-<peer>`). This replaced the former hardcoded
primary/secondary string branches. `peer` is empty for standalone.

### Step: Resolve role + peer

Emits `role=` / `peer=` to `$GITHUB_OUTPUT`. The former inline case-guard against a
stale binary corrupting the output file is unnecessary now: an `llz` too old to know
`env resolve` fails cleanly with "unknown command".

---

## Job: `bootstrap` — Bootstrap OpenBao

### Why there is no `needs: resolve`

This job self-resolves HA role/peer in its first step (same `llz env resolve`, same
container image) instead of serializing behind the standalone `resolve` job's
container spin (~30–40s on the critical path). `resolve` still exists — and now runs
CONCURRENTLY — for the standby jobs below (`provision-peer-ca` / `fetch-standby-ca` /
`reprovision-peer-ca`), which consume its outputs across job boundaries.

### The `timeout-minutes` budget

45m = the old bootstrap 30m + the converge gate's 15m: the final convergence poll
(formerly its own `converge` job) now runs at the tail of THIS job, reusing the one
cluster-access/ACL cycle instead of paying a fresh container-init + checkout +
kubeconfig fetch. With `bootstrap_cluster` the in-cluster bootstrap phase (apl-core
install + the `wait-apl-pipeline` gate) runs at the head of this job too: 70m.

**Measured** (passing cold e2e, run `29658429694`): this job takes 13m24s end to end —
bootstrap-cluster 342s, the post-seed nudge 94s, the OpenBao App assert 96s, converge
66s, the whole assert suite 42s. So the timeout is ~3x observed reality on the 45m path.

**The invariant that matters:** no SINGLE step's budget may exceed this timeout, or a
genuinely slow run is killed by GitHub with no verdict — losing both the step's own
error message and the `always()`/`failure()` diagnostics at the job tail (Argo state
capture, and the control-plane ACL revoke that keeps a runner IP from being left in the
LKE-E allowlist). `wait-apl-pipeline` broke that invariant badly: 6600s of stage
budgets, 110 minutes, inside a 70m job. It is now 3300s.

**What is NOT claimed:** that the budgets SUM to less than this. They do not, and
making them would trade real robustness for a property that never binds — the steps are
sequential worst cases that are never all consumed (13m24s measured against ~147m of
ceilings). If two slow steps ever do coincide, the axe still lands; that is a knowingly
accepted residual, not an oversight.

### Job-level `env`

`github.com` is the default host, so `GH_TOKEN` alone authenticates. `GH_REPO` lets
`gh` target the repo directly instead of shelling out to `git` (which fails on
"dubious ownership" in the container's checkout dir). `OPENBAO_SECRETS_WRITE_TOKEN` is
a fine-grained PAT (Actions + Secrets: write).

`LLZ_PHASE_LOG` drives the phase-timeline log
(`docs/designs/e2e-instrumentation.md`): the converge mark plus the report/upload at
job end capture the converge phase; the converge verb additionally reports its
long-pole (last apps to go healthy).

`HA_ROLE` / `HA_PEER` are exported to `$GITHUB_ENV` by the in-job "Resolve HA role +
peer" step (drives the Harbor seed direction — `ci_harbor` reads it — the
`github-dispatch-token` standby skip, and the cross-region CA extract gate).

`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` are the S3 backend creds for the merged
convergence gate's cluster-access `terraform init` (the former `converge` job set these
at job level).

The `available` output gates the parallel `harbor` + `converge` jobs: both need OpenBao
initialized/unsealed (the seeds + root token) before they can do anything.

### Steps: Resolve HA role + peer / Export HA role

Two steps: the verb emits `role=` / `peer=` step outputs; the export step promotes them
to job-wide env so every later step's `env.HA_ROLE` reference (and the `ci_harbor` env
read) keeps working unchanged.

### The cluster-bootstrap phase (`bootstrap_cluster=true` only)

MERGED here from `llz-terraform.yml`'s former standalone `bootstrap-cluster` job: it
ran on the same cluster with the same `infra-<region>` environment immediately before
this job, so standing it alone only bought a second container-init + checkout +
kubeconfig/ACL cycle (~1.5–2m of pure overhead on the e2e critical path) — the same
rationale that folded the `converge` job into this one. The steps are verbatim from
that job (see its header comments in git history); only the kubeconfig moved: they now
use the default `$HOME/.kube/config` the shared cluster-access step writes, instead of
a job-private `$RUNNER_TEMP` path.

### Removed step: the "first-party chart pins are published to GHCR" preflight

A preflight ran between the env-secret verification and the tfvars render. It could
never work from this checkout and is removed.

Its goal was sound — a pinned `llz-*` chart whose bumped version is not in GHCR fails
only at Argo sync time, so catching it early is worth doing. But the pins are not in
the instance: `platform-apl/` and `kubernetes-charts/` live in the TEMPLATE and reach
the cluster through the terraform modules, fetched by `terraform init` in the APPLY job
— a different job with a different checkout. This job sees only the rendered instance,
whose `apl-values/` is a README and a `values.yaml`. So the scan found zero pins and
reported "every chart published" having verified none, on every run.

That vacuous pass is now a hard failure (the check refuses to pass having checked
nothing), which is correct behavior in the wrong place. The real verification runs
template-side in release-e2e, where the pins actually are, with `--publish-if-missing`
to publish anything absent.

### Step: Render tfvars from the spec

Regenerates the gitignored cluster tfvars from the spec so `wait-cluster-ready` below
can read `node_count` (its pool-Ready target).

### Retired: the `infra-<region>` env-secret preflight block

A preflight surfaced missing GHA env secrets in seconds rather than 30+ min into the
workflow when a seed step exits 0 with an `::error::` and `BOOTSTRAP_ERRORS=true`. Each
missing secret produced its own error annotation so all of them showed up in the run UI
at once. Secrets already verified by later step-scoped preflights
(`OPENBAO_SECRETS_WRITE_TOKEN`, `HARBOR_URL`) were NOT re-checked to avoid duplicate
annotations; the preflight covered the ones whose absence would otherwise only surface
at the seed step itself.

The `LOKI_S3_*` / `HARBOR_REGISTRY_S3_*` half was RETIRED with the secrets themselves:
`llz ci mint-bootstrap-objkeys` mints the keys directly — the bucket preflight is the
remaining gate.

### Step: Pre-flight — verify object-storage buckets/keys exist

The GH-secret presence check only confirms the S3 secrets are PRESENT — not that they
are VALID. If object-storage was never applied (or was reverted), those secrets can be
stale, pointing at a deleted key, and seeding them produces `InvalidAccessKeyId` at
runtime (no logs in Loki, Harbor pushes fail) with no signal. `terraform.yml`'s
`apply-object-storage` now runs on every cluster bootstrap, so this is
defense-in-depth: confirm the `platform-loki-<region>` /
`platform-harbor-registry-<region>` scoped keys exist on Linode before seeding.
Non-fatal on a transient API hiccup.

### Step: Cluster access (kubeconfig + runner ACL)

github.com hosted runners have a dynamic egress IP not pre-included in the LKE-E
control-plane ACL; open it before any `kubectl`, revoke at the end.
`register-runner-lease` leases the IP in the `firewall-runner-acl` ConfigMap so the EAA
controller doesn't evict it mid-run — required because the merged convergence gate at
the tail of this job polls for up to 10m (formerly the separate `converge` job set this
lease on its own cluster-access).

### Steps: Wait for cluster API ready (first node) / Assert the full node pool joined

Split gate: install apl-core as soon as the apiserver answers and ONE node is Ready
(the install is SSA + CRDs + a helm release whose operator pod needs a single
schedulable node), and assert the FULL statically-sized pool only after the bootstrap —
by then the remaining nodes have had the whole apl-pipeline wait to join, so on a
healthy day the assert is instant, and on a bad day (Linode capacity/quota, a ≥16-char
`node_pool_label` wedging join) it still fails the job with the same message it always
did. Serializing the full-pool wait BEFORE the install paid node-join variance on the
critical path for no correctness gain.

The deferred assert passes `--tfvars` so `node_count` is read and the whole pool must be
Ready before the OpenBao phase proceeds.

### Step: Bootstrap cluster (apl-core + Kyverno + Argo bridge)

The whole in-cluster bootstrap in one native command (see
`tools/cmd/llz/ci_bootstrap_cluster.go`): read the live coredns IP, inject the runtime
secrets into the committed apl-values, SSA the StorageClass + namespaces,
`helm upgrade --install` apl-core, race the two Kyverno policies concurrently with the
apl-pipeline readiness gate, then SSA the platform-bootstrap AppProject + Applications.
Idempotent, so a re-run is safe; the chart version is read from the spec.

### Removed: duplicate Argo diagnostics after the cluster-bootstrap phase

Argo diagnostics for a failed bootstrap are NOT dumped here: since the
cluster-bootstrap and converge jobs were folded into this one, the job-tail "Diagnose
convergence failure" step — `if: failure() || cancelled()` — already fires on a
`bootstrap_cluster`-phase failure and runs the same `llz ci diagnose-argocd`. Having
both meant every red run dumped Argo state twice, each capped at 10 minutes.

Similarly, apl-core bring-up timing (image pulls + apl-operator logs) is collected once
at the job tail by "Report bootstrap timing", which runs before the ACL revoke while
cluster access still exists; the `Pulled` events are well within their TTL by then. The
phase marks complete the timeline.

### Removed: the OpenBao serving-TLS bootstrap seed

The out-of-band self-signed `openbao-tls` seed that used to live here has been REMOVED.
`openbao-tls` is now issued by the stable, self-signed cert-manager CA `openbao-ca`
(`platform-apl/manifest/cert-manager/raw/openbao-bootstrap-ca.yaml`), which has no
OpenBao dependency. cert-manager issues `openbao-tls` before OpenBao starts — the
StatefulSet pod simply waits in `ContainerCreating` for the Secret mount — and the
serving CA never changes, so there is no mid-bootstrap CA rotation, no OpenBao reload,
and no ESO CA re-read.

Deliberately NOT re-seeding a self-signed cert here: doing so would race cert-manager
and re-introduce the very rotation this change removes (the seed's self-signed CA would
later be replaced by `openbao-ca`). If OpenBao is stuck `ContainerCreating` on
`openbao-tls`, the fix is to ensure the `openbao-ca` ClusterIssuer is Ready, not to
hand-seed a cert.

The raft `retry_join` pod-FQDN SAN requirement is satisfied by the `dnsNames` in
`openbao-tls-cert.yaml`, so raft TLS forms on first boot.

### Removed: the OTel Collector serving-TLS bootstrap seed

The `llz ci gen-bootstrap-tls` step that used to hand-seed
`platform-otel-collector-tls` here (a throwaway runner-generated CA + cert, replaced
in-place by the custom-ca issuer later) has been REMOVED — the same anti-pattern the
`openbao-tls` seed removal fixed. The cert is now issued from FIRST boot by the stable,
self-signed cert-manager CA `otel-bootstrap-ca`
(`platform-apl/components/observability/otel-bootstrap-ca.yaml`, synced by the
platform-bootstrap Application), with the per-env `otel.<env>.internal` SAN rendered by
`llz render`. No workflow step, no mid-bootstrap CA rotation. If the Secret is missing,
the fix is to ensure the `otel-bootstrap-ca` ClusterIssuer is Ready, not to hand-seed a
cert.

### Step: Seed OpenBao static auto-unseal key

The chart's `seal "static"` stanza mounts the `openbao-unseal-key` Secret at
`/openbao/seal/unseal.key`; without it the StatefulSet pods sit in `ContainerCreating`
(a missing Secret volume waits, it does not crash-loop), so this must run BEFORE "Wait
for OpenBao pods to be running".

Idempotent and never-rotating: an existing Secret is left untouched; on a namespace
rebuild the key is restored from the `infra-<region>` `OPENBAO_SEAL_KEY` secret; a
first-ever bootstrap generates it, persists it to `infra-<region>` for DR (needs
`GH_TOKEN`, the job-level secrets-write PAT), and prints an offline-backup banner —
losing this key loses the data (recovery keys authorize `generate-root` but cannot
decrypt the root key).

### Step: Assert the OpenBao Application reached the cluster

**Fail-fast gate.** On every wedge of the 2026-07-04 outage (PR #142) the OpenBao
Application was never created — the platform-bootstrap sync was stuck waves earlier —
and the 600s pod wait below burned its full budget with nothing to say. Assert the
Application exists first (a healthy sync creates it within ~2 min); when it doesn't,
this fails in ≤4 min WITH the parent sync's `operationState` message. Terminal parent
failures short-circuit immediately; a Running-but-retrying sync (by-design first-boot
transients) gets the full window.

The Argo Application is `llz-openbao` (its name in `llz-cluster-foundation` /
`llz-argo-bootstrap-apps` values). NOT `platform-openbao` — that is only the Helm
RELEASE / StatefulSet identity (pods `platform-openbao-0..2` in the `llz-openbao`
namespace). The gate polled the release name for its first three releases (added
PR #143), so it never matched an Application and timed out on every full e2e — the last
green run predated the gate.

### Step: Wait for OpenBao pods to be running

`kubectl rollout status` waits for pods to be Ready, but a freshly deployed OpenBao
comes up uninitialized — and an uninitialized pod reports sealed, which the readiness
probe correctly marks as not-Ready (keeps it out of Service endpoints). Readiness only
happens AFTER this workflow runs `bao operator init` (the pods then auto-unseal from the
static seal key) — so `rollout status` would time out on every first bootstrap. The init
step only `kubectl exec`s into the pods, which just needs them Running — hence
`wait-pods` polls phase, not readiness.

**600s budget.** Cold-bootstrap path: PVC binds → Linode Volume create (~30s/PVC × 3
raft pods serially = 90s under `WaitForFirstConsumer` binding) → image pull (~20s) →
cert mount → Running. StatefulSet default `podManagementPolicy` is `OrderedReady` so
pods come up one at a time; pod 0 reaching Running has to clear its Volume create
before pod 1 starts. 300s was borderline on slow-Linode days and produced spurious
failures we then had to retry.

### Step: Ensure OpenBao initialized + unsealed + root token

`llz ci bao-ensure-ready` collapses the former steps (status probe, first-init
preflight, init, wait-for-auto-unseal, load-root, regen-root-if-revoked, availability
check) into one detect → choose-a-path → re-verify command:

| State | Path |
| --- | --- |
| uninitialized | init (recovery keys) + wait for the pods to auto-unseal from the static seal key |
| initialized + sealed | wait for the pod(s) to self-unseal (no keys) |
| initialized | validate the loaded root token, regen via the recovery-key quorum if a prior run revoked it |

It writes `available=<bool>` to `$GITHUB_OUTPUT` (the gate every later step checks) and
re-exports the effective `OPENBAO_ROOT_TOKEN` to `$GITHUB_ENV`. The first-init
secrets-write-token preflight is folded in (it fails fast if `GH_TOKEN` is unset on an
uninitialized cluster). `bao-init`'s no-manual-raft-join contract (chart `retry_join`
handles membership; a duplicate join 500s) and the `VAULT_SKIP_VERIFY` local-hop
rationale live in the command. `GH_TOKEN`/`GH_REPO` come from the job-level env; the
recovery keys + loaded root token are the `infra-<region>` secrets (empty on a first
bootstrap — init mints and persists them).

### Step: Pre-flight — resolve Harbor URL for configuration

`HARBOR_URL` is the registry hostname buildah pushes to / images pull from (stored in
OpenBao as `registry_host`) — NOT how this job reaches Harbor's API (that's the
`kubectl port-forward`). It is NOT an operator-required input: apl-core already serves
Harbor at `harbor.<cluster_domain>` (the gateway HTTPRoute host), so the command
defaults to that, read from the cluster-bootstrap tfvars. `vars.HARBOR_URL` (the
job-level env) still overrides; it fails only if neither is available.

### Step: Configure OpenBao

All enable commands are idempotent (`|| true`). Write commands are upserts.

### Removed: the `post-apl-bootstrap` kustomize apply

The `kubectl apply -k post-apl-bootstrap` step that used to run here
(OpenTelemetryCollector, whose CRD apl-core's helmfile registers) has been REMOVED: the
CR now rides the observability component
(`platform-apl/components/observability/otel-collector.yaml`) at a late sync-wave. The
platform-bootstrap App's `SkipDryRunOnMissingResource` + retry budget absorb the
CRD-registration race the imperative step existed to sidestep; the server-side dry-run
lint path that motivated the split no longer exists (CI builds the manifest tree
client-side).

### Step: Seed OpenBao KV bootstrap paths

One step runs every generic `bao-seed` path from the `bootstrapSeeds()` table in
`llz ci bao-seed-all` (`tools/cmd/llz/ci_bao_seed_all.go`):

* `secret/infra/github-dispatch-token` (harbor-ready PostSync hook)
* `secret/cert-automation/github-token` (cert-automation ExternalSecret)

`secret/harbor/admin`, `secret/grafana/admin` and `secret/otel/ingress` are no longer
seeded here — ESO writes them in-cluster via PushSecrets. `secret/loki/object-store` +
`secret/harbor/registry-s3` moved to the `mint-bootstrap-objkeys` step: the keys are
minted, not relayed. `secret/linode/api-token` moved to the `mint-bootstrap-pat` step:
it now holds the NARROW in-cluster PAT, minted — the broad provisioning PAT never
enters the cluster.

Per-seed on-missing modes, idempotency guards, and summary notes are UNCHANGED — they
moved from step flags into the table. A missing `env:`/`k8s:` source defers per its mode
(`BOOTSTRAP_ERRORS` for `github-dispatch-token` on active/standalone; the rest skip with
a summary note); a `kv-put` failure aborts before the remaining seeds, exactly as a
failed inline step did. `HA_ROLE` (job env) drives the `github-dispatch-token` standby
skip. `OPENBAO_ROOT_TOKEN` comes from `$GITHUB_ENV` (the ensure step).

### Step: Mint + seed object-storage keys

`llz ci mint-bootstrap-objkeys` mints the region's scoped keys via the Linode API
(labels/buckets mirror the rotation table: `platform-loki-<region>` spanning the
chunks/ruler/admin buckets, `platform-harbor-registry-<region>` on its bucket) and seeds
`secret/loki/object-store` + `secret/harbor/registry-s3` directly, `rotated_at`-stamped
so the in-cluster `linodeCredRotator` adopts them on its own cadence.

REPLACES the TF-minted keys and the whole GitHub relay (`stash-env-secret` →
`LOKI_S3_*`/`HARBOR_REGISTRY_S3_*` → `bao-seed` / `seed-harbor-registry-s3`): the
credentials never transit GitHub, and key lifecycle has one owner (mint here, rotate
in-cluster). Idempotent — already-seeded paths are skipped, so a re-bootstrap never
clobbers a rotator-minted key. `obj_cluster` comes from the object-storage tfvars — the
source of truth for which OBJ cluster TF provisioned the buckets into (NOT derivable
from the env name). The downstream ESO wiring (`harbor-registry-s3` /
`loki-object-store` ExternalSecrets → K8s Secrets → `REGISTRY_STORAGE_S3_*` env vars /
`singleBinary` `extraEnvFrom`) is unchanged.

### Step: Mint + seed the in-cluster Linode PAT

Same one-owner shape as the object-storage keys: `llz ci mint-bootstrap-pat` mints the
NARROW in-cluster PAT (domains/object_storage/volumes rw + linodes/vpcs ro + firewall
rw — label `llz-incluster-<region>`) with the broad provisioning PAT and seeds
`secret/linode/api-token` directly, `rotated_at`-stamped.

Every in-cluster Linode consumer (volume-labeler, the cred-rotator's minting credential,
cidr-firewall, and apl-core's DNS-01 webhook + ExternalDNS via the
`kyverno-dns-rotating-token` mutation) reads this one rotating token; the broad PAT is
CI/Terraform-only and never enters the cluster. Monthly rotation:
`llz-secret-rotation.yml` → `llz ci rotate-incluster-pat`. Idempotent — an
already-seeded path is skipped (a rotation-minted token, or the legacy broad-PAT seed on
a pre-split cluster, is never clobbered; the next rotation converges the path to a
narrow token).

### Step: Seed broad-PAT rotator token (when enabled)

The in-cluster broad-PAT rotator (`platform-apl/components/broadPatRotator`) mints its
successor with the CURRENT broad PAT, which it reads from `secret/linode/broad-pat` via
ESO. This step seeds that path from `LINODE_API_TOKEN` — the `account:read_write` CI
token, i.e. the minting privilege the rotator needs — `rotated_at=0` so the first tick
is due; `--skip-if-present` keeps re-runs from clobbering a value the rotator has since
rotated.

Gated on the rendered carved App file so ONLY the one deployment that enables the
component (the broad PAT is account-wide) ever holds the token: every other instance
renders no such file and skips, so no cluster carries the broad PAT unless it owns the
rotation. This also unwedges convergence — without the seed the rotator's ExternalSecret
sits in `SecretSyncError` and its carved App (`llz-broad-pat-rotator`) stays Degraded.

### Step: Seed standby Harbor robot credentials (standby only)

A standby peer has no in-cluster Harbor (the active/standalone path is the in-cluster
`harbor-robot-provisioner` CronJob — see "Harbor provisioning moved in-cluster" below).
It seeds `secret/harbor/{robot,pull-robot}` from the repo-level `HARBOR_*` secrets the
active's provisioner published; each not-published-yet state is a summary note + clean
exit (re-run after the active's provisioner has run). Sits before the nudge so the
force-sync picks the fresh seeds up immediately.

### Step: Nudge Argo CD to converge secrets (post-seed)

The `openbao` ClusterSecretStore and its ExternalSecrets (loki/harbor obj-storage) could
not reach Ready until OpenBao was unsealed and the object-storage KV paths were seeded.
`nudge-argo` (1) forces an immediate sync of the two apps that own them — re-triggering
the sync if an earlier first-boot race already drove it to a terminally-failed state
(Argo CD does NOT auto-retry a failed sync to the same revision; `selfHeal` only
corrects drift after a successful one) — then (2) bumps a revalidation annotation on the
ClusterSecretStore and WAITS for it to go Ready: the converge precondition only CI can
assert, because only CI knows seeding just finished.

It no longer force-syncs the ExternalSecrets itself (secrets-before-apps Phase 3): the
in-cluster `es-store-recovery` reconciler lane fires on the very not-Ready→Ready
transition (2) triggers, and covers PushSecrets too. Never fails the bootstrap.

### Step: Wait for Harbor registry rollout

`harbor-registry` mounts the `harbor-registry-s3` Secret via `secretKeyRef`, so it stays
in `CreateContainerConfigError` until that Secret exists. It only appears after the
registry-S3 KV path is seeded AND the `es-store-recovery` lane force-syncs the
ExternalSecret on the store going Ready — which is why it's checked HERE rather than in
the earlier control-plane "Wait for Harbor readiness" gate (gating on it there always
hit the 2m rollout timeout on fresh bootstraps).

`wait-harbor --registry-only` now polls the real rollout budget and self-reports
non-fatally: it prints a `::warning::` and exits 0 if the registry hasn't rolled out in
time (the convergence gate is the hard check, and the release-e2e image push exercises
the registry plane proper). So this step is honestly green and no longer needs
`continue-on-error` painting a check over a "timed out … exit 1".

### Removed: Seed Gitea backup S3 credentials

The step that wrote `secret/gitea/backup-s3` was removed in the apl-core anti-pattern
cleanup. The corresponding bucket + key + outputs + ExternalSecret + CronJob are all
gone too. See `apl-values/primary/values.yaml` `apps.gitea` for the rationale (apl-core
`extraVolumes` list-replace clobbered custom-ca; integration blocked pending a kustomize
post-renderer follow-up).

### Step: Extract standby CA cert

When bootstrapping a standby cluster, extract its OpenBao TLS CA cert and pass it as a
job output so the `provision-peer-ca` job can create the `openbao-peer-tls` Secret in the
active peer's cluster, establishing cross-cluster trust so `VAULT_SKIP_VERIFY` can be
false for standby operations. Standalone/active deployments have no peer, so this is
skipped.

The CA cert is deliberately NOT `::add-mask::`'d: it is public material (the
certificate, not the key), and the runner empties masked values in JOB outputs — the
same constraint documented in `linode-credentials/action.yml` ("raw token cannot cross
job boundaries"). Masking here would hand `provision-peer-ca` an empty `ca_b64` and
silently provision an empty `ca.crt`.

Non-fatal: a standby whose `openbao-tls` isn't up yet just skips peer-CA provisioning
(`ca_available=false`) rather than failing the bootstrap.

### Removed: Verify ExternalSecrets synced

The `Verify ExternalSecrets synced` step (and its backing
`template-scripts/verify-externalsecrets.sh`) was deleted in the convergence-contract
anti-pattern cleanup. It existed to `kubectl annotate ... force-sync=$(date +%s)` every
ExternalSecret because ESO's 24h refresh cache could hold a `Ready=False` from before
the workflow seeded the source path. Replaced by Argo CD `health.lua` for
ClusterSecretStore + ExternalSecret (set via apl-core values at
`apl-values/<env>/values.yaml::apps.argocd._rawValues.configs.cm`) plus
`retry: backoff` on every Argo Application. The `llz ci converge` polling gate is what
now confirms ESO has caught up. See `docs/architecture/convergence-contract.md`
anti-pattern #3.

### Step: Audit PVCs against encrypted-Retain StorageClass

The Kyverno ClusterPolicy at
`terraform-iac-bootstrap/cluster-bootstrap/manifests/kyverno-pvc-encrypted-storage-class.yaml`
rewrites `linode-block-storage(-retain)` → `block-storage-retain` at admission. The TF
install (`wait_for_kyverno_crd` → `kubectl_manifest`) races apl-core's helmfile that
creates harbor/gitea/keycloak/CNPG PVCs; the policy's mutating webhook has a 30–90s
readiness lag after CRD registration. Any PVC admitted during that window lands on
`linode-block-storage` (unencrypted, Delete reclaim) and persists silently — Kyverno
does NOT background-migrate existing resources.

This step lists every PVC not on `block-storage-retain` and emits `::warning::` lines so
the operator can decide whether to delete+recreate the affected workloads (forcing PVC
re-admission with the policy now active). Does NOT fail the workflow — the cluster is
still functional, just less secure than intended.

### Step: Revoke root token

Defaults to `always()`-equivalent: revoke on success AND failure (cleanup is
unconditional — fail-closed for credential hygiene). An explicit
`preserve_root_on_failure=true` dispatch input lets the operator opt into leaving the
token usable across iterative bootstrap retries, so they don't have to burn a fresh
quorum-regenerate per failed attempt. Success always revokes regardless of the flag.

### The final convergence gate (formerly the separate `converge` job)

MERGED here from its own job: convergence runs serially after this bootstrap on the same
cluster with the same `infra-<region>` environment, so a standalone job only bought a
second container-init + checkout + kubeconfig fetch (~1.5m of pure overhead on the e2e
critical path). Running it inline reuses THIS job's cluster-access/ACL/lease.
`llz ci converge` exits 0 (converged) or 1 (budget exhausted / check error); per
`docs/architecture/convergence-contract.md` this gate is what makes "the workflow
passed" mean "the cluster works". All steps use the container's default kubeconfig
(`$HOME/.kube/config`, written by cluster-access) — the same one the bootstrap kubectl
steps use.

**Behavior change vs. the old split:** a convergence failure now fails THIS job, so the
standby-only `provision-peer-ca` (which `needs: bootstrap`) is skipped on a converge
failure instead of still running. That HA path is not exercised by release-e2e
(standalone); deferring peer-CA provisioning until a clean bootstrap is the intended
trade.

### Step: Preflight — confirm the cluster admits the llz CronJob

Clears the `verify-llz-image-signature` race before the gate.
`verify-llz-image-signature` (Enforce, sync-wave -15) blocks the in-cluster llz CronJobs
until Kyverno can verify the keyless cosign signature on the llz image; on a freshly
built `main` sha the Sigstore/Rekor + GHCR signature can lag the cluster's first
admission attempt, failing the platform-bootstrap sync (Argo backs off to 5m retries and
does not auto-retry a failed sync to the same revision), so confirm the cluster can admit
the CronJob before the converge poll adjudicates. The CronJob manifest is read from the
RENDERED instance checkout (source of truth). Best-effort probe only — the re-sync it
used to trigger is the argo-nudge reconciler lane's job now.

No `available` gate (unlike the seed/configure steps): the old standalone `converge` job
ran on bootstrap success REGARDLESS of OpenBao availability, so an OpenBao that never
came up still surfaces as a convergence FAILURE here rather than a silently-skipped
gate. Default `success()` gating preserves that — it runs unless an earlier step failed.

**Why 8 attempts × 15s (2m).** Between the original 20×15s (5m) and the 3×10s this was
briefly cut to. The cut was reasoned from "nothing acts on the result, so a warning-only
probe needs one attempt" — true, but it missed WHAT the retries wait for. On a cold
bootstrap the CronJob is not admissible yet because its NAMESPACE does not exist: e2e run
`29664222266` warned `namespaces "llz-pat-rotator" not found` after exhausting 3 attempts
in 46s, where the 20-attempt budget had admitted on attempt 1. So the retries were riding
out platform convergence, not a Sigstore lag, and cutting them turned a probe that passes
into one that reliably reports a false alarm. 2m covers namespace creation without
re-adding the 5m tail.

`kubectl apply --dry-run=server` makes Kyverno admit the real CronJob — the server
dry-run runs the autogen-cronjob `verifyImages` rule (and every other admission policy)
without persisting.

**No re-sync nudge** at the end of the step (secrets-before-apps Phase 3). Argo doesn't
fast-retry a sync that terminally failed on the earlier (pre-signature) admission denial
— but the in-cluster argo-nudge reconciler lane watches for exactly that and re-triggers
`phase=Failed` apps within seconds, continuously, so a CI-side one-shot adds nothing.
This step is now purely the dry-run probe: it produces the actionable warning, and the
converge poll is the verdict.

### Removed: the one-shot pre-converge "Realign argocd-redis on WRONGPASS" step

The converge poll's own reactive realign (`ci_health.go`: on a detected
WRONGPASS/NOAUTH split it restarts argocd-redis once per run) already covers this —
including a split present BEFORE converge, which it catches on poll 1 — so the pre-step
only realigned ~one poll earlier while putting ~25 lines of warm-cluster (`KEEP_CLUSTER`)
debug logic on the shared production bootstrap path. The durable fix still belongs in
apl-core (restart-on-rotation / static redis password); until then the reactive self-heal
is the single compensator.

### Step: Kick harbor-robot-provisioner (skip the */5 first-tick wait)

`llz-cert-automation`'s rollout chains on the `harbor-robot-provisioner` CronJob seeding
`secret/harbor/robot` → the `harbor-docker-config` ExternalSecret syncing → the rollout.
Left to the schedule, the first tick is up to 5m out and ESO adds its 1m
`refreshInterval` — the dominant always-pay tail the converge budget absorbs. Force one
tick NOW (a one-off Job from the CronJob, after harbor-core is Available) and force-sync
the ExternalSecrets, collapsing that cron-paced tail to the Job's own runtime.
Best-effort by design (the verb always exits 0): the cron schedule remains the standing
safety net and the converge poll is the verdict. Skipped on standby (no in-cluster
Harbor).

### Step: Wait for cluster to converge — why `--budget 1200`

Explicit `--budget` bounds the convergence poll, well below the job's `timeout-minutes`
(45). It is NOT "anything longer is a real stall": the tail of a fresh bootstrap is
`llz-cert-automation` legitimately rolling out. It chains on the
`harbor-robot-provisioner` CronJob (schedule `*/5`) seeding `secret/harbor/robot` → the
`harbor-docker-config` ExternalSecret syncing (`refreshInterval` 1m) → the
cert-automation rollout. In the worst case (the CronJob's first tick up to 5m out) that
tail runs ~10–15m after the primary converge, so a 10m ceiling was marginal and flaked
the poll on a legit slow rollout (Progressing, not stalled). 20m absorbs the worst-case
tail while still catching a genuine stall well inside the 45m job timeout — and the kick
step makes the worst case rare rather than routine.

### Step: (e2e) assert suite — 6 gates + 2 diagnostics, in parallel lanes

E2E validation gate, folded in from `release-e2e.yml`'s former `validate` job: that job
dispatched `cluster-health.yml` only to run `llz ci converge` (redundant with the poll
above) + `llz ci assert-loki`. Running `assert-loki` HERE — inside the converge that
already holds cluster access — drops that whole extra job (its container-init + checkout
+ kubeconfig/ACL cycle) from the e2e critical path. Default-off, so normal instance
bootstraps are unchanged; only the release-e2e provision dispatch passes
`assert_loki=true`. Uses the job's default kubeconfig (`$HOME/.kube/config`), same as
the poll.

These gates ran back-to-back (~80s serial best case; minutes when
`assert-health-workflow` retries), but they are independent read-mostly checks against
the already-converged cluster, so they fan out as background lanes and the step fails if
ANY GATING lane fails. Wall clock = slowest lane (typically a health-workflow run).

Per-lane rationale (each verb is unit-tested; details in its Go file):

* **loki** — Loki bootstrapped + S3-backed (the former `validate` job's one net-new
  check, folded here to drop that job's container cycle).
* **scrape+reconciler** — ONE lane, ordered. `assert-scrape-targets` proves every
  landing-zone ServiceMonitor has a live `up` target and each PrometheusRule group
  loaded (the silent un-scraped-CR regression class); `assert-reconciler` then reads
  `llz_reconcile_up`/`_leader`, which the scrape assert just proved fresh. Splitting them
  would race the gauge's first scrape.
* **health-workflow** — submits a one-shot Workflow from the `llz-cluster-health`
  WorkflowTemplate and asserts it Succeeds — the day-2 RUN path (kyverno signature policy
  on the pod, SA/executor RBAC, `health-incluster` verb). SKIPS clean when the component
  is disabled (normal instances).
* **broad-pat** — forces one rotation Job from the weekly CronJob and asserts
  `action=rotated` — exercises mint → OpenBao → GitHub publish → revoke against real
  backends. SAFE: e2e-unique label + `broadPATDeployments=e2e` scope the mint/revoke to
  the e2e PAT family only. Self-gates on the component toggle.
* **wave-vap** — GATING proof that the `llz-wave-health-guard` VAP is BOUND and
  ENFORCING: server-dry-runs a Deployment at sync-wave -5 and requires the guard's own
  denial. This is what makes the static guard's PR-time verdict hold at runtime. It
  replaced the old ~45s live negative-wave enumeration, which was tautological — the Deny
  binding means a flaggable resource could never have been admitted.
* **instance-custom** — GATING proof of the operator escape hatch. The release-e2e
  instantiate seeds a trivial manifest under
  `kubernetes-custom/namespaces/llz-e2e-custom/`, so on a converged cluster the
  `instance-custom` ApplicationSet's git directory generator must have generated
  `instance-custom-llz-e2e-custom` and synced it. `converge` and `assert-loki` gate the
  PLATFORM apps and stay green when the hatch generated NOTHING (the generated App simply
  would not exist) — only an assertion that names it catches that.
* **metric-surface** — report-only (`|| true`) dump of the loki/cortex/otelcol/harbor
  exporter metric NAMES, so error-rate/saturation alerts get written against series that
  actually exist (promtool checks syntax, not existence). The
  `llz_es_`/`llz_reconcile_linode_token` families were also dumped here to feed the
  secrets-before-apps Phase-2 rollout gate; Phase 3 has landed, so that half of the regex
  is gone.
* **alert-eval** — report-only (`|| true`) live evaluation of every deployed alert expr,
  written to the job summary. promtool cannot tell a rule referencing a non-existent
  metric (silent never-fire, DEAD?) from one simply not tripping (ARMED); this can. The
  intent is still to harden to `alert-eval --strict` once today's known-DEAD? alerts —
  real cluster bugs, not rule bugs — are resolved.

**Concurrency safety:** the two MUTATING lanes (health-workflow's Workflow, broad-pat's
Job) touch disjoint namespaces/objects. The lanes that query Prometheus
(scrape+reconciler, metric-surface, alert-eval) each open their own port-forward with
local port `:0`, so kubectl assigns a free port per lane and they cannot collide.

`alert-eval`'s FIRING/ARMED/DEAD?/BROKEN report is the deliverable, so it goes to the job
summary as it did when this ran as its own step.

Three of these lanes (`instance-custom`, `metric-surface`, `alert-eval`) used to run
serially after the suite; all three are read-only against the already-converged cluster,
so they are now lanes of the parallel suite. The runtime wave-health check that ran as
its own step is now the `wave-vap` lane.

### Step: Capture loki-gateway diagnostics (on convergence failure)

On a convergence hard-fail, snapshot the loki-gateway resolver + nginx logs WHILE the
cluster is still up — before the ACL revoke cuts apiserver access. Every command is
`|| true` so it never alters the outcome.

### Step: Diagnose convergence failure (Argo apps + cert-manager CA chain)

Snapshot the Argo CD Application states + the phase1 `platform-app-ca` CA chain before
teardown. `|| cancelled()` so a cancelled run still captures; all probes are best-effort
inside the llz command. Also covers a bootstrap-step failure (this is now one job) — the
reason the old standalone Argo-diagnostics step existed in this job at all.
Belt-and-suspenders `timeout-minutes: 10`: `diagnose-argocd` gates on apiserver
reachability, but bound the step regardless so a hung apiserver can't eat the job budget.

### Job-tail steps: timing, ACL revoke, fail-on-errors

"Fail on bootstrap errors" runs after token revocation so cleanup always happens first.
It exits 1 to surface the error in the GitHub Actions UI when `BOOTSTRAP_ERRORS` was set
by a seed step.

The ACL revoke happens BEFORE the job ends and passes the kubeconfig so the runner-lease
in the `firewall-runner-acl` ConfigMap is released via kubectl before the Linode-API call
cuts apiserver access (we `register-runner-lease` on cluster-access).

Bootstrap timeline → step summary + artifact (best-effort, always-on). With
`bootstrap_cluster=true` this job runs the whole cluster+OpenBao bring-up, so the marks
span `wait-cluster-ready` → `apl-core-install` → `foundation-ready` → `converge`; the
collector adds `image-pulls.json` + `apl-operator.log` to the same dir. The converge verb
also appends its long-pole to the summary. See `docs/designs/e2e-instrumentation.md`.

---

## Removed job: `harbor` — Harbor provisioning MOVED IN-CLUSTER

The `harbor` job that ran between `bootstrap` and `provision-peer-ca` (its own
cluster-access + ACL cycle, a root-token re-acquire via the recovery-key quorum, a
`kubectl port-forward` to harbor-core, `ensure-project`, `provision-harbor-robots`,
smoke) has been REMOVED.

The active/standalone path is now the `harbor-robot-provisioner` CronJob
(`platform-apl/components/harbor/`, `llz ci harbor-provisioner` on the slim llz image):
it talks to `harbor-core.harbor.svc` directly (the port-forward existed only because
`HARBOR_URL` is internal DNS the runner cannot resolve), writes OpenBao through the
scoped `harbor-provisioner` Kubernetes-auth role (no root token), publishes the
repo-level `HARBOR_*` secrets natively over the GitHub API, and smoke-tests every tick.
Robot rotation is now "delete the robot in the Harbor UI" — the next tick recreates and
re-publishes it. The STANDBY half (replicating the active's published credentials) is the
"Seed standby Harbor robot credentials" step in the `bootstrap` job.

---

## Job: `provision-peer-ca` — Provision standby CA in active peer cluster

Runs only after a standby bootstrap that successfully extracted the CA cert. Creates (or
updates) the `openbao-peer-tls` Secret in the active peer's `openbao` namespace so
cross-peer clients can verify TLS when connecting to the standby OpenBao without
`VAULT_SKIP_VERIFY=true`. The peer (the active member of this deployment's `ha_group`) is
resolved from the cluster tfvars.

### Step: Cluster access (peer kubeconfig + runner ACL)

Fetches the active peer's kubeconfig and opens this runner's egress IP in that cluster's
control-plane ACL (revoked at job end). No `llz` needed here.

---

## Job: `fetch-standby-ca` — Fetch standby CA (recovery)

Triggered via `workflow_dispatch` with `reprovision_ca_only: true` and `region` = the
standby deployment. Fetches the CA cert from the standby cluster (self) and passes it to
`reprovision-peer-ca` without running a full bootstrap. Use this to recover from a failed
`provision-peer-ca` without restarting the entire standby bootstrap. No-op unless the
deployment's role is standby.

### Step: Cluster access (standby kubeconfig + runner ACL)

Fetches the standby cluster's (self) kubeconfig and opens this runner's egress IP in its
control-plane ACL (revoked at job end). No `llz` needed.

### Step: Extract standby CA cert

`--required`: this job exists solely to (re)provision the peer CA, so an absent
`openbao-tls` is a hard error here — unlike the `bootstrap` job's twin, which is
non-fatal.

---

## Job: `reprovision-peer-ca` — Re-provision standby CA in active peer cluster

Depends on `fetch-standby-ca`; runs in the active peer's environment. Its cluster-access
step fetches the active peer's kubeconfig and opens this runner's egress IP in that
cluster's control-plane ACL (revoked at job end). No `llz` needed there.
