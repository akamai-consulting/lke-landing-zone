# Convergence contract

**Audience:** anyone touching Terraform, `llz ci bootstrap-cluster`, the bootstrap workflows, the cluster-health script, or an Argo Application in this repo.

This doc defines what "the cluster is done bootstrapping" means, and the three exit codes that every layer of the bootstrap honours.

## The problem we're solving

Before this contract existed, the bootstrap declared completion based on **commands returning**, while the cluster only reached a working state through **async convergence nobody waited for**. Concretely:

- The bootstrap returned success while the apl-core helm install had only confirmed the `apl-operator` Deployment was Ready — its 40-component helmfile pipeline still had 10–15 minutes of work left.
- Every step after that raced an invisible pipeline. The result (when this bootstrap was still the `cluster-bootstrap` Terraform workspace): ~6 polling `null_resource` loops totalling ~40 minutes of timeout budget, each guessing about when something downstream would be ready. That workspace has since been retired — the bootstrap now runs as the native `llz ci bootstrap-cluster` command — but the contract below is unchanged.
- ``llz ci health`` exited `0` for both *"fully converged"* and *"pre-bootstrap, nothing started yet"* (`Phase 0`). The workflow couldn't tell *"done"* from *"half-done"*.
- Soft-fails (`|| true`, `::warning::`, `BOOTSTRAP_ERRORS=true`) accumulated state in OpenBao / GitHub secrets / Harbor while the bootstrap was already known-broken.

The contract below replaces all of that with explicit signals.

---

## The four exit codes

Every "is the cluster ready?" check in this repo — ``llz ci health``, the TF readiness gates, the converge wrapper, any future workflow that needs to ask the question — uses **exactly four exit codes**:

| Exit | Meaning | What the caller should do |
|---|---|---|
| **`0`** | **Converged**. Every required component is Synced + Healthy or Ready, every operator-deferred input is documented as such, and no transient reconciles are in flight. | Proceed. |
| **`2`** | **In-progress**. The cluster is not yet converged, but no hard failure is observable — Argo apps are still applying / Pods are still pulling / Certificates are still issuing / a CRD just landed and its first reconcile loop hasn't run. | **Poll**. Re-run the same check after a backoff. |
| **`1`** | **Hard-failed**. A required component is in a state the reconciler cannot resolve on its own — ImagePullBackOff on an image that doesn't exist, CrashLoopBackOff with `Error` exit code, a Job past `backoffLimit`, a Certificate stuck on `IssuerNotReady` for an Issuer that itself is in a `NotReady` terminal state. | Stop. Operator intervention required. |
| **`3`** | **Apiserver unreachable**. An infrastructure-level blip, not a statement about the cluster's contents — the check could not ask the question at all. | **Retry without spending a hard strike.** Callers MUST distinguish this from `1`; collapsing it into `1` turns every transient apiserver blip into an operator-visible failure. |

### How to classify a check

When writing a new check, the question to ask is:

> *Will time fix this without me doing anything?*

- If yes — and you can describe what time will fix (a reconciler will catch up, a TLS handshake will complete, a Job will retry) — it's exit `2`.
- If no — and you can describe what the operator has to do (provide a missing secret, fix a typo, deal with a payment failure) — it's exit `1`.
- If everything that needs to be true is true — it's exit `0`.

> **Phase 0 (cluster has nothing yet) is exit `2`, not exit `0`.** The cluster will eventually have things if the bootstrap chain keeps running. The previous behaviour of conflating Phase 0 with converged-state is the single change that breaks the "step-and-pray" model: the caller now distinguishes "nothing started" from "everything done".

### What the previous "soft-fail" buckets become

- `EXTERNAL_DEP_APPS` / `EXTERNAL_DEP_WORKLOADS` / `EXTERNAL_DEP_EXTERNALSECRETS` / `NP_EXTERNAL_DEP_NAMESPACES` — these allowlist *known-deferred* operator inputs (e.g. an application-supplied API key, `LINODE_DNS_TOKEN`, a security-scanner token, etc.). Items that match a list entry are **PENDING** (still exit `2`, not `0`) until the operator-supplied input arrives. The lists no longer change exit-`0`-vs-exit-`1` — they only determine whether a not-yet-Ready resource counts as `2` (waiting on a documented operator input) or `1` (broken).
- The previous "DRIFT" bucket (cosmetic OutOfSync drift from `ExternalSecrets` admission defaults, shared `PodDisruptionBudgets`, immutable StatefulSet template diffs) stays — these are exit `0` reportable annotations, not failures.

---

## How the three layers honour the contract

### 1. The bootstrap command (`llz ci bootstrap-cluster`)

`llz ci bootstrap-cluster` returns success **only when the in-cluster bootstrap has reached the hand-off state**, not when the helm install returns. (Terraform now owns day-0 infra only — the cluster, VPC, firewall, node pool, and object-storage buckets; the in-cluster bootstrap that used to be the `cluster-bootstrap` Terraform workspace is this command.)

One loud readiness gate replaces the ~600 lines of imperative polling that used to live in `null_resource.wait_for_argo_application_crd`, `null_resource.wait_for_kyverno_crd`, and friends:

- **`waitAplPipeline`** (`tools/cmd/llz/ci_wait_apl_pipeline.go`, also runnable as `llz ci wait-apl-pipeline`) — waits for the apl-operator's helmfile pipeline by asserting **Argo CD's `argocd-application-controller` is serving, Kyverno's admission controller is Available, and cert-manager's webhook is Available**. Those are the canonical "the platform prerequisites are up" signals; before they're true, applying the bootstrap Application (or letting the helmfile create PVCs) is a race. We gate on these rather than the helm install's built-in wait because that wait only covers the `apl-operator` Deployment, not its downstream pipeline. The gate **fails loud** (non-nil error → the command fails) — no soft-fail-and-continue.

The command applies the bootstrap Argo Application only **after** that gate returns; from there Argo owns the reconcile (its `retry: backoff` rides out the first-boot convergence window), and the deep-convergence verdict is `llz ci converge` (below). The previous pattern — bash polling loops scattered across multiple `null_resource`s that each made up their own answer to "is X ready?" — is replaced by **one shared readiness model**.

### 2. The cluster-health script

``llz ci health`` is the single source of truth for "is the cluster converged?". It is the **only** script that decides exit `0` vs `1` vs `2` — every other script and workflow that needs the answer **calls it** rather than re-implementing.

It takes no mode flag — there is one behavior, and callers distinguish outcomes
by exit code alone. Its only flag is `--fail-on-unhealthy`.

### 3. The converge wrapper

``llz ci converge`` is the "poll until ready" primitive. It:

1. Calls ``llz ci health``.
2. If exit `0` — succeeds.
3. If exit `2` — sleeps `$INTERVAL` seconds (default 30) and re-checks.
4. If exit `1` — re-runs once after `$RETRY_DELAY` seconds (default 60) to absorb transient-but-misclassified failures, then propagates exit `1`.
5. If exit `3` — retries without counting a hard strike (the apiserver was unreachable, so the cluster's state is simply unknown).
6. After `$BUDGET` seconds (default 30 min) of total elapsed time with no exit `0`, gives up with exit `1` and dumps a final diagnostic.

`llz-terraform.yml`'s `bootstrap-openbao` chain calls ``llz ci converge`` at its tail (the former standalone `bootstrap-cluster` and `converge` jobs are folded into the single `bootstrap` job) and treats its exit code as authoritative: a passed workflow now means "the cluster converged within budget", not "every step I happened to run returned 0".

---

## Anti-patterns that violate this contract

If you find yourself writing any of these, stop and reconsider:

1. **A `kubectl wait --for=condition=X` against a CRD that may not exist yet.** It errors `NotFound` immediately and `--timeout` only governs an *existing* resource. Use a real readiness gate — `waitAplPipeline` (existence-poll then condition-wait) is already there.
2. **A new polling step in the bootstrap (a `null_resource` back when it was TF, or an ad-hoc `kubectl wait` loop now).** Almost every case is better solved by letting the bootstrap Argo Application + Argo's reconcile own it. If you genuinely need a new platform-prerequisite gate (e.g., a new CRD-installing component the bootstrap depends on), add a stage to `aplPipelineStages`, not a sibling loop.
3. **`|| true` after a step that performs a write.** That's the pattern that produced multiple `BOOTSTRAP_ERRORS=true` flags in `bootstrap-openbao.yml`. If the write can fail and we want to keep going, the right shape is *classify the failure* (exit-2 vs exit-1) and propagate that — not silently swallow the error.
4. **A new side-controller to drive what an existing reconciler should observe.** Where cert-watcher side-controllers exist because upstream cached config until pod restart, they're tracked for removal as part of a follow-up that addresses CA trust via a normal cert-manager + Argo `health.lua` flow. Don't add a third one.
5. **`exit 0` in a check.** A check returns one of `0/1/2`. If the answer is genuinely "I don't know", that's `2` (the caller polls), not `0` (the caller proceeds).
6. **CI-imperative force-sync / rollout-restart / annotate-to-nudge.** If a reconciler will reach the target state on its own within an acceptable window (ESO immediate reconcile on creation, Argo `retry: backoff`, cert-manager renewal), the nudge is a workaround for an impatient wait, not a real fix. Either widen the wait budget on the downstream `kubectl wait` (the caller already polls), or push the work into the reconciler's natural cadence. Don't paper over reconcile latency with `kubectl annotate force-sync=$(date +%s)`.

---

## See also

- `tools/cmd/llz/ci_bootstrap_cluster.go` — the native bootstrap command: header comment + the ordered flow (the `waitAplPipeline` gate raced against the two Kyverno policies).
- `tools/cmd/llz/ci_wait_apl_pipeline.go` — the loud readiness gate (`aplPipelineStages` + the existence-poll → condition-wait state machine).
- ``llz ci health`` — header comment + the `MODE_*` constants + the helper functions that classify a resource into `0/1/2`.
- `instance-template/.github/workflows/bootstrap-openbao.yml` — header comment + the Branch A / Branch B / Re-configure mode selector, which is the same `0/1/2` shape applied to OpenBao seal state.
- ``llz ci converge`` — the polling wrapper itself.
