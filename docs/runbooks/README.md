# Runbooks — find one by what went wrong

Recovery and rotation procedures. Every file here is a **live procedure**, not a
record — if one describes something the platform no longer does, that is a bug in
the runbook, and `docs-guard` checks the mechanical half of it.

These ship into every instance (`llz ci deliver-docs` keeps `runbooks/`), so they
are read by operators who do not have this repo checked out, often mid-incident.

## By symptom

| What you are seeing | Runbook |
|---|---|
| Your first `llz up` / `llz build` went red and you need to know what to do | [first-build-failed](first-build-failed.md) |
| Standing up OpenBao on a new cluster, or recovering a sealed/half-configured one | [bootstrap-openbao](bootstrap-openbao.md) |
| An in-cluster alert is firing and you need the response for it | [reconciler-alerts](reconciler-alerts.md) |
| A recreated cluster will not converge; the values repo still has the old `apl-<env>` branch | [apl-branch-recreate-wedge](apl-branch-recreate-wedge.md) |
| apl-core is not picking up values, or you need to know how values reach the cluster | [apl-values-propagation](apl-values-propagation.md) |
| Leftover Linode Volumes after a destroy, or a bill for storage nothing is using | [orphan-volume-cleanup](orphan-volume-cleanup.md) |
| Volumes in the Linode UI are all `pvc-<uuid>` and you cannot tell them apart | [volume-labels](volume-labels.md) |
| An apply is refused because it would replace your Object Storage buckets | [bucket-prefix-rename](bucket-prefix-rename.md) |
| An `llz ci assert-*` lane is red in release-e2e and the log is not enough | [e2e-lane-diagnostics](e2e-lane-diagnostics.md) |

## Scheduled / on-demand rotation

| Credential | Runbook |
|---|---|
| Linode API tokens (PATs) and Object Storage access keys | [linode-credential-rotation](linode-credential-rotation.md) |
| The per-cluster `lke-admin` token embedded in the LKE kubeconfig | [lke-admin-rotation](lke-admin-rotation.md) |
| Giving an operator scoped OpenBao **write** access without the root token | [openbao-team-login](openbao-team-login.md) |

## Onboarding an existing cluster

| | |
|---|---|
| Adopting a pre-LLZ Akamai App Platform site — inventory it, then scaffold a matching instance. A **rebuild + migrate**, not an in-place upgrade | [import-apl-site](import-apl-site.md) |

## Adding one

Runbooks are read under time pressure by someone who did not write them:

1. **Open with `**Applies to:**`** — a reader must be able to rule it out in one line.
2. **Say what must never be done**, if there is such a thing, before the steps
   (`lke-admin-rotation` leads with "never `kubectl delete` the token Secret").
3. **Show the command, with the flag that makes it real.** `llz openbao set`
   dry-runs and exits 0 without `--yes`; a runbook that omits it documents a
   procedure that silently does nothing.
4. **Add a row above**, so the next person finds it by symptom rather than by
   guessing the filename.
