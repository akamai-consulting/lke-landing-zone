# Design documents

Working designs: the shape of a change, the alternatives weighed, and the
rollout. Unlike an [ADR](../adr/README.md) — which is a dated record of a
*decision* and is meant to age — a design doc describes **work**, so the useful
question about one is always *"how much of this is real yet?"*

That question is what the **Status** column answers, and it is why every design
in this directory carries a `**Status:**` line directly under its title.

## Status vocabulary

Five values, deliberately few. Prefer the weaker claim when unsure — a design
marked `Partial` that turns out to be finished costs a reader one extra check;
one marked `Shipped` that is not costs them a debugging session.

| Status | Means |
|---|---|
| **Shipped** | Landed on `main` and in effect. The design now describes the system as it is. |
| **Partial** | Some phases landed, others did not. **The status line must say which** — a bare "Partial" is not useful. |
| **Proposed** | Analysed, not built. Nothing in the tree depends on it. |
| **Superseded** | Replaced by a later design or ADR. The status line links the replacement. |
| **Abandoned** | Deliberately not pursued. Kept because the reasoning is worth not re-deriving. |

`docs-guard` exempts this directory from its command/flag check for the same
reason it exempts ADRs: a design that names a flag from a rejected alternative is
doing its job. Links are still checked.

## Index

### Shipped

| Design | What it covers |
|---|---|
| [credential-single-pane](credential-single-pane.md) | One Prometheus/Grafana view of every credential — CI tokens and certificates — with alerts that fire before expiry. Runs as the `credential-single-pane` job in `llz-scheduled-checks.yml`. |
| [day2-incluster-health](day2-incluster-health.md) | kubectl-free health verb + on-demand WorkflowTemplate, so day-2 health does not need a kubeconfig on the operator's machine (#203). |
| [e2e-instrumentation](e2e-instrumentation.md) | Phase timing for the e2e lane, so "where did the time go?" is a query rather than log archaeology (`llz ci phase-timing`). |
| [linode-credential-rotator](linode-credential-rotator.md) | In-cluster ownership of the Linode object-storage key lifecycle; default-on fleet-wide. |
| [linode-pat-dns-consolidation](linode-pat-dns-consolidation.md) | Collapsing the Linode PAT and DNS-token surface. Phases A and B both landed. |
| [apl-core-values-branch-isolation](apl-core-values-branch-isolation.md) | Isolating the apl-core values branch so an instance's values tree cannot be clobbered by the operator's push. |

### Partial — check the status line for which phases

| Design | What landed / what did not |
|---|---|
| [kube-native-reconciler](kube-native-reconciler.md) | Phases 0–2 landed, `llzReconciler` default-on fleet-wide; later phases still rolling out. |
| [secrets-before-apps](secrets-before-apps.md) | Phase 1 (bounded steady-state propagation) landed; Phase 3 landed **partially** — one CI steering call-site deliberately KEPT. Phase 2's store-recovery lane is the open piece. |
| [blast-radius-decomposition](blast-radius-decomposition.md) | The two gates exist (`llz ci wave-dependency-guard`, `wave-health-guard`); the decomposition of the four kustomize Components into independent Applications is the remaining work. |
| [team-scoped-credentials](team-scoped-credentials.md) | Phase 1 shipped (#300), read half completed (#336). |
| [instance-slimming](instance-slimming.md) | Levers 1 and 1.5 landed; levers 2–3 are a staged, e2e-gated rollout. |
| [forge-abstraction](forge-abstraction.md) | `internal/forge` and its code-level phases landed on `feat/forge`; the GHE/GitLab flavours are unbuilt. |
| [shared-managed-postgres](shared-managed-postgres.md) | Terraform module, embedded root and `llz render` wiring landed; the OpenBao seed command and CI jobs are outstanding. |
| [obj-sse-c-gateway](obj-sse-c-gateway.md) | Built, **not deployed** — `spec.components.objProxy` is default-disabled and the activating DNS rewrite is outside the kustomization on purpose. |
| [apl-core-v6-migration](apl-core-v6-migration.md) | apl-core 5.x → 6.x. Pinned to GA `v6.0.0`; validate in lab before any non-lab environment. |
| [apl-core-v61-upgrade](apl-core-v61-upgrade.md) | apl-core 6.0 → 6.1. Baseline moved, pinned to GA `v6.1.0`; same lab-first caveat. |
| [apl-overlay-obj-native](apl-overlay-obj-native.md) | Adopting apl-core-native object storage; runtime behaviour still unconfirmed on a live cluster. |
| [internal-extension-model](internal-extension-model.md) | Phases 1–2 — the declaration model (`tools/internal/extension`) and the first thirteen extensions (`guard-budgets`, `guard-docs`, `posture-at-rest`, `assert-storage`, `reconcile-actions`, `teardown`, `template-sustain`, `import-brownfield`, `obj-encryption`, `guard-charts`, `cluster-access`, `health-sla`, `token-inventory`, plus a registry and `llz extension list`). Nothing is dispatched through it yet: the declarations are inert and all thirteen still run as cobra commands. Action ABI, manifest, per-instance enablement and the remote half did not land; issue #399 sequences them. |

### Proposed — analysed, not built

| Design | What it covers |
|---|---|
| [credential-single-pane-incluster](credential-single-pane-incluster.md) | Moving the credential pane fully in-cluster. Input to the credential-rotation / PAT-window review. |
| [windows-support](windows-support.md) | What native Windows support for `llz` would take, as a tiered spectrum rather than a switch. Tier 0 (WSL2 / Dev Container) already works. |
| [internal-extensions](internal-extensions.md) | The measurement under the decomposition: every non-test file in package `main` assigned to a candidate extension exactly once, with line counts, grants and a suggested order. Nothing is built from it yet. |

### Superseded

| Design | Replaced by |
|---|---|
| [cross-org-reuse-pattern](cross-org-reuse-pattern.md) | [ADR 0003 — vendor actions and bodies into instances](../adr/0003-vendor-actions-and-bodies-into-instances.md), which goes further than this design proposed. |

## Adding one

1. Start with `**Status:**` on the line under the title, using a value from the
   table above. For `Partial`, name the phases.
2. Add a row to the right section here.
3. When the status changes, change **both** — the file and this index. A design
   whose status is stale is worse than one with no status, because the reader
   trusts it.
