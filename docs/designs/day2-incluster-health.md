# Design: in-cluster, CI-agnostic day-2 health (Argo-native)

**Status:** WIP on `feat/day2-argo-health`. Pulled from PR #202 (the cross-org
reuse pattern) after a second critical review — it could not run as designed. This
doc is the roadmap to make it real.
**Relates to:** `instance-template/apl-values/components/clusterHealthWorkflow/`,
`tools/cmd/llz/ci_health.go`, `docs/designs/kube-native-reconciler.md`,
`docs/designs/cross-org-reuse-pattern.md` (§ "Day-2", in #202).

## Goal

Run day-2 checks (health, later rotation/audits) **inside the cluster** as
Kubernetes-native jobs, so nothing CI-vendor-specific runs in the cluster and
there are no GitHub secrets / no `secrets: inherit` — which is exactly what makes
it work across org boundaries. An Argo `WorkflowTemplate` runs `llz` on a
schedule (`CronWorkflow`) and/or on a webhook (`Sensor` + `EventSource`),
authenticated by the workload's **ServiceAccount**. The continuous form of the
same signal is already the `llz-reconciler`; this is the synchronous, triggerable
variant. The portability seam stays `llz ci <verb>` in a container.

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

- [ ] The kubectl-free health verb (option 1) + unit tests on `internal/kube`.
- [ ] Point the WorkflowTemplate at it; drop the WIP marker.
- [ ] Right-size the EventBus (1 replica if it's dedicated to this trigger).
- [ ] Add sync-wave ordering (EventBus before Sensor) if the merge-into-
      platform-bootstrap ordering needs it.
- [ ] **Live-cluster validation** — stand it up on an e2e cluster and confirm the
      CronWorkflow runs the health job green AND the webhook Sensor fires a run.
      This is the gate the round-2 review shows structural/lint checks cannot
      substitute for.

## Non-goals

- Replacing `llz ci health` for the CI/gate path — that stays the kubectl-based
  exit-code source of truth for the terraform converge gate. This adds a
  kubectl-free sibling for the in-cluster job; one predicate library, two
  callers.
- Rotation-style day-2 jobs (OpenBao-writing) — a later increment on the same
  Argo-native substrate, using the retained `openbao-login` primitive.
