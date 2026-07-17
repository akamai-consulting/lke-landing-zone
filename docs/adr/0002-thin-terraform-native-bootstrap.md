# ADR 0002 — Thin Terraform: the in-cluster bootstrap runs natively, not as a TF workspace

- Status: Accepted
- Date: 2026-07-15
- Deciders: platform / LLZ maintainers
- Related: `docs/architecture/convergence-contract.md`,
  `tools/cmd/llz/ci_bootstrap_cluster.go`,
  `tools/cmd/llz/ci_wait_apl_pipeline.go`,
  `tools/cmd/llz/ci_kyverno.go`

## Context

Terraform in this repo did two very different jobs under one roof:

1. **Day-0 infra provisioning** — the LKE cluster, VPC, subnet, node firewall,
   node pool, and object-storage buckets (`terraform-iac-bootstrap/{cluster,vpc,
   object-storage}` + `terraform-modules/llz-{cluster,node-firewall,pool,
   object-storage}`). Here plan/apply/state/drift genuinely pay for themselves:
   these are cloud resources with real identities Terraform is well-suited to own.

2. **In-cluster bootstrap orchestration** — the `cluster-bootstrap` workspace
   (`terraform-iac-bootstrap/cluster-bootstrap` + `terraform-modules/
   llz-cluster-bootstrap`): a `helm_release` for apl-core, a tree of
   `kubectl_manifest` server-side applies (namespaces, StorageClass, the Argo
   bridge AppProject + Applications), `templatefile()` secret injection, and
   `null_resource` local-execs that shelled out to `llz ci wait-apl-pipeline` /
   `llz ci apply-kyverno-policy`.

The second job fought Terraform's model hardest. Its providers were bootstrapped
from a kubeconfig read mid-apply out of the cluster workspace's remote state; its
destroy path needed `terraform state rm` surgery (`llz ci tf-untrack`) to avoid
`helm uninstall` hanging on finalizers and to handle the cluster-already-gone
case; and its `lifecycle { ignore_changes }` blocks existed precisely to hand
ownership of ACLs/firewall/pool off to in-cluster controllers after day-0. The
imperative building blocks it leaned on already lived in Go
(`wait-apl-pipeline`, `apply-kyverno-policy`, `render`, `fetchkubeconfig`,
`destroy-unwedge`, `clear-cluster-secrets`).

## Decision

**Keep Terraform thin — day-0 infra only — and run the in-cluster bootstrap as a
native `llz ci bootstrap-cluster` command driven from CI.** ArgoCD/apl-core own
everything day-2 (they already did, post-seed).

`llz ci bootstrap-cluster` (`tools/cmd/llz/ci_bootstrap_cluster.go`) is a faithful
port of the retired workspace, in the same exec-seam style as the rest of `llz ci`:
read the live coredns ClusterIP, inject the four secrets-only runtime placeholders
into the committed apl-values, server-side-apply the block-storage StorageClass +
`apl-operator`/`argocd` namespaces, `helm upgrade --install` apl-core, then run the
loud `waitAplPipeline` readiness gate **concurrently with** the two race-ahead
Kyverno policies, and finally apply the `platform-bootstrap` AppProject +
Applications. It is idempotent (server-side apply + `helm upgrade --install`), so
CI re-runs it on every apply; the apl-core chart version is read from
`spec.cluster.bootstrap.aplChartVersion`.

Supporting decisions (see the PR's plan for the full rationale):

- **New-clusters-only.** No migration of existing clusters' `cluster-bootstrap`
  state — the in-cluster resources are ArgoCD/apl-core-owned and keep reconciling;
  the orphaned state is harmless. Validated on a fresh `keep_cluster` e2e. (Same
  precedent as the OpenBao static-seal redesign.)
- **Idempotent re-apply owns upgrades.** `helm upgrade --install` with the
  spec-pinned version every run replaces the old `ignore_changes = [version]`
  model: bumping the spec version + re-dispatching applies the upgrade.
- **Loki admin password is first-install-only** (read back from the stored helm
  values on upgrade) so it doesn't churn now that there is no TF state to hold it
  stable.

## Consequences

- **Destroy is simpler and safer.** No `terraform destroy` / `state rm` for this
  layer, and no provisioner that could fire against a live cluster. The two
  still-needed best-effort cleanups (`destroy-unwedge`, `clear-cluster-secrets`)
  run in a slim `pre-destroy-cluster` CI job before the cluster delete, which
  reaps every in-cluster object with the cluster.
- **The convergence contract is unchanged in substance.** `bootstrap-cluster`
  returning `0` still means "every bootstrap resource placed AND the apl pipeline
  reached the hand-off state", enforced by the loud `waitAplPipeline` gate; the
  deep-convergence verdict remains `llz ci converge` at the tail of
  `llz-bootstrap-openbao.yml`.
- **The Kyverno race is preserved.** The two policies are dispatched concurrently
  with the readiness gate (not serialized after it), exactly as the TF
  `depends_on = [helm_release.apl]` (not `apl_pipeline_ready`) encoded — the
  window that beats apl-operator's unmutated PVC creation. Guarded by a dedicated
  ordering unit test.
- **Lost:** the `terraform plan` preview for this layer. Mitigated by the
  command's deterministic ordering, its `--dry-run`, and the retained
  `diagnose-argocd` on failure.
- **Removed:** `terraform-iac-bootstrap/cluster-bootstrap`,
  `terraform-modules/llz-cluster-bootstrap`, the embedded `cluster-bootstrap`
  tfroot, and the now-dead `llz ci tf-untrack` + `internal/terraform/untrack.go`.
  The offline apl-values var-contract guard (`llz ci validate-apl-values`) now
  checks against the Go `bootstrapValuePlaceholders` constant instead of parsing
  the deleted `main.tf`.
- **The `apl-<env>` values branch is bootstrap-owned when absent, operator-owned
  once populated.** apl-core v6's operator does NOT self-create
  `otomi.git.branch` — it deadlocks pulling a missing ref, and once its
  installation status is `completed` it is reconcile-only and never
  re-bootstraps values into an emptied branch (both verified live). So
  `bootstrap-cluster` seeds the branch EMPTY before the helm install, re-arms
  the installer when it re-seeded on an already-installed cluster, and blocks
  hand-off until the operator's first push lands.
- **Watch-item — skip-if-present strands OpenBao secret SCHEMA growth on reuse.**
  `mint-bootstrap-objkeys` and `bao-seed --skip-if-present` no-op a whole KV path
  when one `presentField` is already set. KV v2 writes replace the entire secret,
  so a reused cluster seeded under an OLD field set never gains fields ADDED later
  (e.g. a new key in `harborRegistryS3Fields`) — a fresh cluster gets them, the
  reused one silently keeps the stale shape. Not currently firing; when a
  skip-guarded path's field set grows, either widen its `presentField` to the new
  key or re-seed that path on reuse. (Latent; same class as the loki
  first-install-password reuse.)
- **Watch-item — rebuilt clusters (destroy → recreate, same env).** Destroy does
  not delete `apl-<env>`, so a rebuilt cluster's fresh installer pulls the OLD
  cluster's operator-written env tree (including SealedSecrets sealed for the
  old cluster's key) before force-pushing its own bootstrap over it. The
  release-e2e instantiate sidesteps this by deleting the stale branch; real
  instances rebuilding an env should do the same (delete `apl-<env>` before
  re-applying) until apl-core's behavior on inherited env trees is validated.
