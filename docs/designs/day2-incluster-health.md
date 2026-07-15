# Design: in-cluster, CI-agnostic day-2 health (Argo-native)

**Status:** MERGED. The kubectl-free verb + on-demand WorkflowTemplate landed in
#203 (validated end-to-end on a real cluster — the `assert-health-workflow` gate
went green in release-e2e run 29345530071). #206 trimmed it to on-demand only
(dropped the scheduled CronWorkflow — see "Triggers" below). Originally pulled
from PR #202 (the cross-org reuse pattern) after a second critical review found it
couldn't run as designed; this doc records how it was made real.
**Relates to:** `instance-template/platform-apl/components/clusterHealthWorkflow/`,
`tools/cmd/llz/ci_health.go`, `docs/designs/kube-native-reconciler.md`,
`docs/designs/cross-org-reuse-pattern.md` (§ "Day-2", in #202).

## Goal

Run day-2 checks (health, later rotation/audits) **inside the cluster** as
Kubernetes-native jobs, so nothing CI-vendor-specific runs in the cluster and
there are no GitHub secrets / no `secrets: inherit` — which is exactly what makes
it work across org boundaries. An Argo `WorkflowTemplate` runs `llz`
**on demand** — `argo submit --from workflowtemplate/llz-cluster-health`, or the
unit-tested `llz ci assert-health-workflow` gate — authenticated by the workload's
**ServiceAccount**. The continuous form of the same signal is already the
`llz-reconciler`; this is the synchronous, triggerable variant. The portability
seam stays `llz ci <verb>` in a container.

**Why not ARC (in-cluster GitHub runners):** it embeds a GitHub-Actions-specific
runner controller in every cluster — the opposite of abstracting the pipeline.

## The blocker (why it was pulled) and the fix

**`llz ci health` is a `kubectl` orchestrator** — it shells out to `kubectl`
throughout ([ci_health.go](../../tools/cmd/llz/ci_health.go): `kubectlReachable()`,
`kItems("get", "jobs", "-A")`, …). The slim `llz` image is
`gcr.io/distroless/static` — **no kubectl, no shell**. So an in-cluster
`WorkflowTemplate` running `llz ci health` on that image cannot start.

Two options, best-first:

1. **Kubectl-free exit-code health verb (intended).** Build a new `llz`
   subcommand that computes the convergence verdict over `internal/kube` (the
   hand-rolled REST client, no kubectl) instead of shelling out. Most of the
   pieces already exist kubectl-free in the reconciler:
   - `reconcile_convergence.go` lists Argo Applications via `internal/kube` and
     classifies them with the SAME `internal/health.ClassifyArgoApp` predicate
     `llz ci health` uses → the 0/1/2 convergence code.
   - `reconcile_health.go` reads ESO store / cert-manager Certificates / OpenBao
     pods via `internal/kube`.
   The follow-up factors the resource fetches `ci_health.go` does over kubectl
   (jobs, pods, storageclasses, …) onto `internal/kube`, feeds the existing
   `internal/health` predicates, and returns the convergence-contract exit code.
   Then it runs in the slim image as-is, consistent with the whole in-cluster
   stack (reconciler/rotator/harbor-provisioner are all kubectl-free).
2. **Run in a kube-capable image.** Point the WorkflowTemplate at an image that
   already has kubectl + `llz`. Note: there is **no such image published for
   in-cluster use today** — the GH workflows use `KUBE_IMAGE`/`TF_IMAGE` (repo
   *variables*, not usable from a kustomize manifest), and adding kubectl to the
   *shared slim* image bloats every other slim-image workload. So this needs a new
   image + a copier token, which is more machinery than option 1.

**Decision: option 1.** It's the right long-term shape and reuses code the
reconciler already proved.

## Other round-2 findings (fixed on this branch)

- **Missing EventBus** — Argo Events requires an `EventBus` in the Sensor's
  namespace; none shipped (the argoEvents component installs only the controller +
  CRDs). Added `eventbus.yaml` (native NATS `default` bus in `llz-argo-events`).
- **RBAC gap** — `llz ci health` does `kubectl get jobs -A`; the ClusterRole was
  missing `batch/jobs`. Added.
- **NP / role incoherence** — the `cluster-health` OpenBao role was bound to a SA
  whose NetworkPolicy blocks OpenBao :8200, so nothing could exercise it. Removed:
  the health workflow is **kube-only** (no OpenBao). `llz ci openbao-login` +
  `internal/openbao.JWTLogin` stay as standalone auth primitives for the future
  rotation-style day-2 jobs, which will each bring their own SA + scoped
  bao-configure role + OpenBao egress rule.

## Remaining work (the PR)

- [x] **The kubectl-free health verb (option 1)** — `llz ci health-incluster`
      ([ci_health_incluster.go](../../tools/cmd/llz/ci_health_incluster.go)):
      builds the in-cluster client, classifies Argo Application convergence via
      the shared `convergenceReport` (factored out of `reconcile_convergence.go`,
      same `health.ClassifyArgoApp` predicate), and exits 0/1/2/3. `--fail-on-
      unhealthy=false` is report-only. Unit-tested (`convergenceReport` +
      `convergenceExit`, and the reconciler gauge still passes on the shared core).
- [x] **Point the WorkflowTemplate at it** (`ci health-incluster`); WIP marker
      downgraded to "needs live validation".
- [x] **Fold in the supplementary signals** — DECIDED: won't-do. Convergence
      (Argo Application) is the right scope for the on-demand check; the
      reconciler already emits the ESO store / cert-manager / OpenBao-seal gauges
      continuously (`reconcile_health.go`), so duplicating them in the on-demand
      verb adds nothing. Revisit only if a concrete day-2 report needs them
      synchronously.
- [x] **Webhook trigger dropped** (see below) — no EventBus/NATS to right-size or
      sync-wave-order anymore.
- [x] **Live validation (kind)** — stood the component up on a local kind cluster
      and drove it end-to-end: the verb (5 exit-code cases + a negative RBAC test →
      403→3), the real distroless image as a pod (auto-mounted SA → Succeeded), and
      the actual WorkflowTemplate via `argo submit` (Succeeded green on a converged
      cluster, Failed exit-1 in gate mode on a degraded one). It caught two bugs
      lint/kustomize can't see — a missing `command:` (Argo emissary) and missing
      `workflowtaskresults` executor RBAC — both fixed + re-validated.
- [x] **Real-instance e2e wiring** — `release-e2e.yml` now enables the component in
      the `e2e` env (`llz env set e2e components.argoWorkflows.enabled=true
      components.clusterHealthWorkflow.enabled=true`), so converge validates the
      DEPLOY path (kyverno admits the WorkflowTemplate/RBAC CRs, Argo
      reconciles them), and a new **`llz ci assert-health-workflow`** step in
      bootstrap-openbao's converge (same `assert_loki` e2e gate) submits a one-shot
      Workflow from the template and asserts it Succeeds — validating the RUN path.
      The verb SKIPS (exit 0) when the WorkflowTemplate is absent, so it stays inert
      on a normal instance. Confirmed locally: `env set` + `llz render` wire the
      component into the values kustomization and `kubectl kustomize` builds it; the
      pure verb helpers are unit-tested. Awaiting a green release-e2e run to close
      the environment-integration loop.
- [x] **Green release-e2e run** — DONE (run 29345530071): the `assert-health-
      workflow` gate submitted a Workflow from the template on the real apl-core
      cluster and it reached `Succeeded`, so the two things kind specifically
      CANNOT verify are now exercised on a real cluster:
      - **NetworkPolicy enforcement** — kindnet doesn't enforce NPs. The workflow
        pod carries `app.kubernetes.io/name: llz-cluster-health` (verified on kind),
        so the NP selects it; its egress (DNS + apiserver 443/6443) covers both the
        verb's Application read AND the emissary's `workflowtaskresults` write — but
        enforcement is unproven until a Cilium/default-deny cluster runs it.
      - **Kyverno image-signature policy** — the pod runs `ghcr.io/<upstream_org>/llz`,
        so kyverno-verify-llz-image-signature gates it like every llz workload
        (verify keyless sig + mutate to digest). The signed image passes and the
        reconciler already runs the same image under the same policy, so this should
        be a non-event — but kind has no Kyverno to prove it.

## Triggers — on-demand only (scheduled cron dropped, webhook dropped)

The component is now **on-demand only**: a `WorkflowTemplate` run via
`argo submit --from workflowtemplate/llz-cluster-health` or the unit-tested
`llz ci assert-health-workflow` gate. Neither a scheduled `CronWorkflow` nor a
webhook adapter ships.

**Scheduled `CronWorkflow` — dropped (does not earn its keep).** It ran
`llz ci health-incluster` every 6h, report-only. But the `llz-reconciler` already
samples the **identical** `convergenceReport` (same `health.ClassifyArgoApp`
predicate) every ~30s and emits it as the **alertable, historized** Prometheus
gauge `llz_convergence_state` (+ `_apps_failed`/`_pending`), alongside richer
ESO/cert-manager/OpenBao gauges. A 6-hourly, non-alertable log line of a verdict
the reconciler already publishes finer-grained is pure redundancy — it even
invites "which do I trust?" confusion and double maintenance. So the scheduled
cron is gone; the reconciler owns the continuous signal, and this template owns
the on-demand/synchronous check the gauge isn't (plus the substrate the day-2
rotation/audit jobs reuse). **Re-add a CronWorkflow ONLY** for a deployment that
runs **neither** the reconciler nor Prometheus — and then set
`fail-on-unhealthy=true` so a bad run is a visible **Failed Workflow**, not an
unread log.

**Webhook trigger + its NATS EventBus — dropped** (earlier). NATS/the EventBus
existed ONLY to carry a webhook event from an `EventSource` to a `Sensor` (the
"triggerable by GitHub/GitLab/curl" adapter) — a 3-pod StatefulSet for an optional
trigger `argo submit` already covers. The Sensor, EventSource, EventBus (NATS),
and the sensor ServiceAccount/RBAC are gone; the component depends on
`argoWorkflows` alone (not `argoEvents`). Re-add only if a concrete
external-webhook use-case appears (then run NATS at 1 replica).

## Non-goals

- Replacing `llz ci health` for the CI/gate path — that stays the kubectl-based
  exit-code source of truth for the terraform converge gate. This adds a
  kubectl-free sibling for the in-cluster job; one predicate library, two
  callers.
- Rotation-style day-2 jobs (OpenBao-writing) — a later increment on the same
  Argo-native substrate, using the retained `openbao-login` primitive.
