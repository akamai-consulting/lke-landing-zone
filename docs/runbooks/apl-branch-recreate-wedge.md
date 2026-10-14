# Stale `apl-<env>` Branch Wedge on Cluster Recreate — Runbook

**Applies to:** any deployment whose cluster is **destroyed and recreated** while its
values repo still carries the operator's per-env `apl-<env>` branch (values-branch
isolation, [ADR](../designs/apl-core-values-branch-isolation.md))
**Symptom class:** bootstrap convergence timeout — `llz ci bootstrap-cluster`'s pipeline-ready gate (formerly `null_resource.apl_pipeline_ready`)
fails with `crd/applications.argoproj.io did not appear within 15m0s — apl-operator
helmfile likely stalled`
**First seen:** v0.0.25 e2e (run 29446982026, 2026-07-15); mechanism confirmed live on
a kept cluster the same day

---

## Why this happens

apl-core's operator commits its rendered values tree **and the platform
SealedSecrets** to `apl-<env>` on every reconcile. The branch is self-created on
first reconcile — but
nothing resets it when the **cluster** is destroyed and recreated. The recreated
cluster's operator then clones the stale branch and finds the *previous* cluster's
SealedSecrets — sealed by a sealed-secrets keypair that died with that cluster. The
new controller can never decrypt them, so the operator wedges at
`waitForSealedSecrets` **before helmfile installs anything**: no stage-01 charts
(kyverno / ESO / cert-manager), an empty `argocd` namespace, and the
pipeline-ready gate times out on its first stage.

## How to recognize it

All of these together (each individually is ambiguous):

- `wait-apl-pipeline` fails at **stage 1** (`crd/applications.argoproj.io` never
  appears) — not a later stage.
- The `apl-operator` pod is **Running 1/1 with 0 restarts** (it is wedged, not
  crashing) and the cluster is freshly recreated.
- `kubectl -n apl-operator logs deploy/apl-operator` shows
  `Applying SealedSecret manifests` followed by
  `Waiting for N sealed secrets to be decrypted: …` and no further progress
  (`llz ci diagnose-argocd` dumps this log).
- `kubectl -n sealed-secrets logs deploy/sealed-secrets` loops
  `ErrUnsealFailed: no key could decrypt secret`.
- The values repo's `apl-<env>` branch head predates the current cluster
  (`gh api repos/<instance>/branches/apl-<env>` — compare the commit date with the
  cluster's creation time).

## Remediation (proven, ~2 minutes)

The branch is **machine-owned** per the ADR — nothing but the operator authors it, so
deleting it is always safe; the operator self-creates a fresh one (with secrets sealed
by the *current* cluster's key) on its next reconcile.

```sh
# 1. Delete the stale branch
gh api -X DELETE "repos/<owner>/<instance-repo>/git/refs/heads/apl-<env>"

# 2. Bounce the operator so it re-clones immediately (instead of waiting out
#    its internal retry/timeout loop)
kubectl -n apl-operator rollout restart deploy/apl-operator
```

Observed recovery on a live wedged cluster: helmfile stage-01 installs resumed
within ~60s of the restart; `crd/applications.argoproj.io` appeared ~2min in;
`ErrUnsealFailed` stopped immediately. If the wedge was hit during a `terraform
bootstrap run, re-run `llz ci bootstrap-cluster` after the operator converges — the
failed pipeline-ready gate re-runs and passes.

## Prevention

- **E2e:** the release-e2e instantiate step deletes `apl-<E2E_ENV>` right after its
  `main` force-push (this repo, `release-e2e.yml`), so every run starts branchless.
- **Real deployments:** deliberate destroy/recreate of an env must include deleting
  its `apl-<env>` branch (step 1 above) before the new cluster's bootstrap. The
  ADR's validation checklist covers fresh-branch self-creation but not recreate —
  closing that gap in the template (e.g. bootstrap resetting the branch when
  the cluster id changes) is an open follow-up; until then this is an operator step
  on the teardown path.
