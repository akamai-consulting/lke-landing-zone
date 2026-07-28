# ADR 0007 — The app-delivery boundary: LLZ ships the platform contract, not the delivery chart

Status: **Accepted — adoption DECLINED; the gaps close as platform contract instead.**
Date: 2026-07-28
Relates: [ADR 0005](0005-managed-app-platform.md), [ADR 0006](0006-managed-default-apps.md);
PR #342 (which deferred this question explicitly), PR #344 (the field lessons).

## Context

The downstream `gsap-apl` instance built a `managed-apps` delivery chart during its
first workload build-out: `apps/<name>.yaml` per application, globbed by an Argo
`ApplicationSet`, rendering a Deployment, Service, Istio `VirtualService`, an
`ExternalSecret` for the app's own secrets, a buildkit `WorkflowTemplate` that
builds from git and pushes to Harbor, and — opt-in per app — a Crossplane
`Database` (provider-sql) and `Bucket`/`Key` (provider-linode).

It works, and getting it to work cost **25 fix commits in 72 hours**. Roughly half
of those were not about app delivery at all; they were the platform failing to
hold up its end (see the audit that produced #342 and #344). That is what makes
adoption tempting: every next instance will rebuild this scaffolding and re-hit the
same landmines, and "make the next downstream instance setup easier" is exactly the
goal that surfaced the chart in the first place.

PR #342 deliberately left the question open rather than answering it by reflex,
noting that adopting the chart "would put app delivery inside LLZ's boundary for
the first time."

## Decision

**LLZ does not adopt the `managed-apps` delivery chart.** The boundary in
[AGENTS.md](../../AGENTS.md) stands unchanged:

> The product is out of scope — only the platform is reusable. A sibling team's
> application workloads are the product, so they are *not* helmified here; the
> reusable unit is the platform that runs them.

**The test.** A thing belongs in LLZ when *every* instance needs it and its shape
does not depend on what the instance runs. A Harbor project an app can push to
passes. A `Deployment` template with `replicaCount` and a `VirtualService` does not
— its shape is the product's.

Applying that test to the chart splits it cleanly:

| Piece | Verdict |
|---|---|
| Deployment / Service / VirtualService templates | **Product.** Out. |
| Per-app build `WorkflowTemplate` (buildkit → Harbor) | **Product.** Out — see "the version-churn tax" below. |
| Crossplane `Database` / `Bucket` / `Key` | **Product.** Out; LLZ ships no Crossplane (#342). |
| A Harbor project + robot an app can push AND pull | **Platform.** In — already shipped. |
| An OpenBao subtree an app's ESO can read | **Platform.** In — already shipped (#336). |
| Argo Workflows CRDs available when asked for | **Platform.** In — already shipped (#342). |
| An `imagePullSecret` in the workload namespace | **Platform.** In — **open, see below.** |
| A StorageClass whose PVCs actually mount | **Platform.** In — documented (#344). |

### Why not adopt it anyway

**The version-churn tax has no home.** The delivery chart's hard parts are not the
Deployment — they are buildkit's rootless mount requirements, Argo Workflows'
vendored-types gap, upjet's connection-secret key prefixes, and provider-sql's
Aiven quirks. Each is a moving dependency with its own release cadence. LLZ's
publishing contract versions charts independently but pins Terraform modules and
the CLI to **one umbrella tag**; a delivery chart would drag buildkit, argo-workflows
and two Crossplane providers into that lockstep, where a security bump in any of
them becomes an LLZ release.

**It would reverse #342's Crossplane call for free.** `database.enabled` /
`objectStorage.enabled` are the chart's most valuable features and both are pure
Crossplane. Adopting the chart adopts the providers, which #342 declined on the
stated grounds that they are app-platform domain.

**One instance is not a pattern.** The chart encodes gsap-apl's choices — Istio
routing, buildkit over Kaniko/ko, build-from-git over build-elsewhere-and-push,
`main`-only revisions. A second instance with a different CI story would fork it
immediately, and LLZ would own both.

**The expensive knowledge is already transferable, and now transferred.** What cost
gsap-apl 72 hours was not typing the templates; it was discovering *why* the push
401'd, why the pod had no AppArmor field, why `{}` would not clear a `nodeSelector`.
That is what #342 and #344 folded back, and it helps the next instance whether or
not it uses this chart.

## Consequences

- The next instance still writes its own delivery mechanism. **Accepted** — with the
  landmines documented, that is template work, not archaeology.
- LLZ owes a *stated* platform contract, not just working code. The table above is
  that contract; the "In" rows are what an app author may assume exists.
- gsap-apl's chart stays gsap-apl's, free to move at its own pace.

## The one gap this leaves open

**Nothing distributes an `imagePullSecret` to a workload namespace.** `secret/harbor/pull-robot`
is seeded and published, and [docs/secrets.md](../secrets.md) now says plainly that
LLZ "does not build one for you" — corrected in #342 from a claim that it did.

This one sits **on the platform side of the test**: every instance that runs any
workload out of the platform Harbor project needs it, and its shape does not depend
on what the workload is. gsap-apl hit it as a plain 401 with `no basic auth
credentials` after the push already worked.

Deliberately **not** resolved in this ADR, because it is a real design choice and
not a typo:

1. **Leave it manual** (status quo). Documented recipe; the operator creates the
   Secret and either patches the namespace default ServiceAccount or names it per
   pod. Zero LLZ surface; every instance pays the same small tax and can get it
   wrong the same way.
2. **Ship an ESO template per declared team namespace.** LLZ already knows the team
   namespaces (`spec.teams`) and already renders the dockerconfigjson from robot
   creds for cert-automation — this is the same ExternalSecret pointed at a
   different namespace. Small, and it fits the existing pattern.
3. **Patch the default ServiceAccount** in each team namespace. Most convenient for
   app authors, most surprising: it changes the meaning of every pod in the
   namespace, including ones that never wanted the platform registry.

Option 2 is the likely answer — it closes the gap without changing pod semantics —
but it needs its own PR and a decision about whether a team namespace is the right
scope. Tracked here rather than silently omitted.

## Alternatives considered

**Adopt the chart wholesale.** Rejected above.

**Adopt a stripped subset** (Deployment + Service only, no build, no Crossplane).
Rejected: the stripped part is the part that is trivial to write and most likely to
be wrong for the next instance. All the value is in the parts that fail the test.

**Ship it as an unsupported example under `docs/`.** Tempting, and genuinely
useful — but an example that is not built or tested by any gate rots, and a rotted
example that *looks* official is worse than none. If this is revisited, it should be
a real chart in a real gate or nothing. The lessons in
[docs/lessons-learned.md](../lessons-learned.md) carry the transferable part today.
