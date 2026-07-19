# `llz-cluster-health` — maintainer rationale

`instance-template/.github/workflows/llz-cluster-health.yml` is the reusable
(`workflow_call`) cluster-health check. It is **vendored verbatim into every
customer instance by copier**, alongside the composite actions it calls, so the
job runs self-contained with no cross-repo checkout. An instance ships a
~20-line caller stub that owns the trigger surface and vendors this body; the
health tooling (`llz ci health`) is baked into `vars.TF_IMAGE`. See
`docs/adr/0003-vendor-actions-and-bodies-into-instances.md` for the
surface-reduction pattern.

Because the YAML is copied into instances where it can never be updated in
place, long-form maintainer archaeology — incident IDs, PR numbers, "we tried X
and it failed because…" — lives here in the template repo instead of in the
workflow body. This document is the archive; the inline comments are the 3am
debugging aids.

---

## Two modes, one body

One body serves both uses of the instance's `cluster-health.yml` caller — they
differ only by flags:

- **recovery / diagnostic** (the all-defaults dispatch): report-only; the job
  summary is the deliverable, and the job never fails.
- **gate mode** (OPERATOR-dispatched): `fail-on-unhealthy` + `assert-loki`.
  NOTE: release-e2e no longer dispatches this — its former `validate` job was
  folded into `bootstrap-openbao`'s converge, which runs the assert suite
  inline.

## Inputs and secrets

### `template-ref`

The template release the instance is rendered from — `llz upgrade` re-pins it.
It is **unused by this workflow's jobs** (everything resolves locally, from the
vendored copy). It is declared only because the caller stub passes it and
`workflow_call` rejects undeclared inputs.

### `vars` and secrets resolution

`vars.TF_IMAGE`, `vars.TF_STATE_BUCKET` and `vars.TF_STATE_ENDPOINT` resolve
from the **caller's** repo/org variables automatically; secrets are passed by
the caller via `secrets: inherit`. The CI image is pulled with the built-in
`GITHUB_TOKEN` (`github.actor`) — no GHCR PAT — so the images must be public or
grant the caller repo package-read access.

---

## Job: `health`

### Step: Checkout the instance (TF roots)

A **single** checkout, at the workspace root. The `fetch-kubeconfig` composite
action hard-codes `working-directory: terraform-iac-bootstrap/cluster` — the
INSTANCE's TF layout — and a composite action's `working-directory` is resolved
against the workspace root and cannot be overridden by the caller. Because the
caller instance is at the workspace root and vendors `.github/actions/`, the
composite actions resolve locally with no separate template checkout.

### Step: Cluster access (kubeconfig + runner ACL + llz)

Fetches the kubeconfig, opens this hosted runner's dynamic egress IP in the
LKE-E control-plane ACL before the kubectl steps (revoked at job end by the
final `if: always()` step), and installs `llz`.

### Step: Cluster health

The summary is always written **before** the exit code is honoured, so a
failing health run still leaves its report in the job summary.

**The 180s apiserver wait loop.** The cluster-access step opened THIS runner's
egress IP in the LKE-E control-plane ACL moments earlier, and a fresh ACL change
takes tens of seconds to take effect — so probing immediately spuriously trips
`llz ci health`'s exit-3 ("apiserver unreachable") gate on a cluster that is
actually up. The provisioning gate (`llz ci converge`) already absorbs this
transient by retrying exit-3 against its budget; the loop gives the one-shot
validate gate the same tolerance so the two agree. It is bounded by wall clock
(`SECONDS`) rather than iteration count so a genuinely dead apiserver still
fails fast enough — well inside the job timeout and before `assert-loki`.

**Why gate mode polls instead of snapshotting.** Gate mode runs
`llz ci converge --budget 300 --interval 20` rather than taking a one-shot
snapshot, so a transient — an ephemeral CronJob pod caught mid-
`ContainerCreating` (e.g. `argo-resync-nudger` firing on schedule), or a
phase/ACL blip — can't fail the gate moments after the provisioning converge
already passed. That is the same tolerance the provisioning gate has, with a
short budget so a genuinely broken cluster still fails well inside the job
timeout. Report-only mode deliberately keeps the one-shot snapshot: it is a
point-in-time diagnostic, not a gate.

> **Planned move.** The ~47-line health `run:` block and the 180s apiserver wait
> loop are slated to move into the `llz` binary in a later phase. Keep inline
> notes there short; the reasoning belongs in this document.

### Step: Argo CD sync diagnostics (best-effort)

Best-effort Argo CD sync diagnostics — the `operationState` / failed-resource
data that `llz ci health` doesn't surface. It never alters the job outcome
(`|| true` throughout, `if: always()`).

`timeout-minutes: 10` caps the diagnostics' own runtime. `diagnose-argocd`
itself gates on apiserver reachability (~10s clean skip when unreachable), but
the follow-up `kubectl` loop over the carved Applications is unbounded — the cap
keeps a hung apiserver from stacking per-probe dial timeouts against the job
budget.

The trailing `llz ci alert-eval` / `llz ci prom-metrics` pair is a live alert +
metric-surface snapshot: which deployed alerts are FIRING/ARMED/DEAD?/BROKEN
right now, and what the exporter metric surface looks like. This surfaces
silently never-firing rules on prod day-2, not just in the e2e.

### Steps: Assert Loki / Assert observability + reconciler + wave-health

Both are gated on the same `assert-loki` input — the E2E validation gate. They
assert the observability pipeline is **wired**: every landing-zone
`ServiceMonitor` has a live `up` target and every `PrometheusRule` group is
loaded, and that no deployed alert is DEAD?/BROKEN (`alert-eval --strict`). The
best-effort `alert-eval` in the `always()` diagnostics block above stays
report-only for recovery dispatches; these two are gating and run only under the
E2E validation input.

### Step: Revoke control-plane ACL for this runner

`if: always()` — closes the egress-IP hole opened by cluster-access, whatever
the outcome of the health steps.
