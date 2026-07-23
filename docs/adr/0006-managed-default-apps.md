# ADR 0006 — Managed default apps: enable apl-core's harbor/loki/grafana/kyverno via the values branch; layer LLZ extras on the managed installs

Status: **Proposed / in progress** (driven by the PR #306 managed-only pivot + the live-convergence e2e)
Date: 2026-07-22
Relates: [ADR 0005](0005-managed-app-platform.md) (the managed pivot). Supersedes 0005's "managedApps is operator opt-in" framing.

## Context

On a Linode MANAGED App Platform cluster (`apl_enabled=true`), a fresh cluster installs a **minimal apl-core** — the platform core (keycloak, argocd, cert-manager, ESO, sealed-secrets, gitea, istio, prometheus-operator, cnpg) — and leaves the optional apps (**harbor, loki, grafana, tempo, kyverno, trivy, velero, falco, argo-workflows/events**) OFF, enabled per-cluster in the App Platform Console.

PR #306 first modeled those optional apps as **operator opt-in** (`spec.cluster.bootstrap.managedApps`), and gated the LLZ extras that ride on them (`harbor` robot-provisioner, `observability` glue, `imageSignature` policy) on that declaration.

Two things forced a rethink:

1. **The Kyverno wedge (e2e, 2026-07-22).** LLZ's supply-chain control is a Kyverno `ClusterPolicy` — a CR of Kyverno's OWN CRD (`kyverno.io/v1`). If Kyverno isn't installed, ArgoCD can't even admit it (`no matches for kind ClusterPolicy`) and the whole `platform-bootstrap` app-of-apps wedges. This is UNLIKE the harbor/observability extras, which are standard resources (ExternalSecret / CronJob / ServiceMonitor / PrometheusRule / Certificate) whose CRDs are in the core **regardless** of whether the harbor/loki/grafana *apps* are enabled — those apply cleanly and are merely inert until their app exists. The distinguishing property is: **a CR of an opt-in app's own CRD cannot live in the always-on tree.**

2. **Product intent.** harbor/loki/grafana/kyverno should be **installed by default** on an LLZ managed cluster (not opt-in), and LLZ should **use apl-core's managed installs** of them (never install a colliding second copy — an operator enabling apl-core's copy in the Console must not collide with an LLZ-shipped one).

## Decision

**On managed, LLZ makes harbor/loki/grafana/kyverno (the LLZ-required optional apps) enabled-by-default in apl-core, and layers its extras on apl-core's installs.**

Concretely:

1. **Enable the apps in apl-core's config**, so apl-core (apl-operator) installs them itself. apl-core reconciles its config from a **git values branch** — and (per the standing requirement, see Open Question 1) LLZ keeps managed apl-core pointed at a **github.com values branch it controls** (via `APL_VALUES_REPO_TOKEN`, Contents:write), NOT the in-cluster gitea default. So enabling an app = **committing `apps.<name>.enabled: true` to that branch** → apl-operator installs it. This is the self-install values-push model applied to managed; no apl-api JWT dance, no gitea-admin credential.
2. **Use apl-core's installs.** LLZ ships only the glue (ExternalSecrets, the harbor robot-provisioner, the image-signature ClusterPolicy) — never its own harbor/loki/grafana/kyverno chart.
3. **Drop the per-app opt-in gating.** With the apps default-on, the `ManagedConditionalOn` gating (PR #306) collapses: the `harbor`, `observability`, and `imageSignature` components emit **always** on managed. `imageSignature` (the Kyverno ClusterPolicy) is safe again because Kyverno's CRD is now guaranteed present.
4. **Ordering is absorbed by Argo.** LLZ commits `apps.*.enabled` early; apl-operator installs the apps (CRDs land) over the following minutes; the LLZ extras that reference those CRDs are `SkipDryRunOnMissingResource=true` with the load-bearing retry budget (40 @ 90s), so they converge once the CRDs appear rather than hard-failing.

`spec.cluster.bootstrap.managedApps` is retained as the **declarative set of apps LLZ enables** (defaulting to `[harbor, loki, grafana, kyverno]`), no longer an operator opt-in — LLZ reconciles apl-core to it, and the `llz-reconciler` can re-assert it against Console drift.

## Mechanism: writing `apps.<name>.enabled`

apl-core's config is Git-as-database (apl-api commits to it; apl-operator reconciles from it). Two write-paths were evaluated:

| Path | How | Verdict |
|---|---|---|
| **A — commit to the github.com values branch** (CHOSEN) | LLZ writes `apps.<name>.enabled: true` into the values tree on the github branch apl-core is pointed at, using `APL_VALUES_REPO_TOKEN`. apl-operator reconciles. | Simple auth (a git push LLZ already does), no new secret, no coupling to apl-api's runtime API. Requires apl-core to be pointed at a github branch (Open Q1). |
| **B — drive apl-api (the Console's path)** | `PUT`/`PATCH` the settings/apps endpoint on `apl-api` (`otomi` ns). | apl-api is OpenAPI-first (`src/openapi/*.yaml` in linode/apl-api), **JWT-authenticated** (`Authorization` + `Auth-Group` headers), Git-as-DB. The JWT (normally minted by an oauth2/Keycloak login) is the blocker for automation in-cluster. Kept as the fallback if Path A's github-branch pointing proves infeasible. |

Path A is preferred because it reuses the credential + push LLZ already performs and avoids coupling to apl-api's runtime auth. LLZ already reads apl-core's config in-cluster today (`discoverKeycloakIssuerFromCluster` reads the `otomi/otomi-api` ConfigMap's `SSO_ISSUER`), so the read side is proven; this adds the write side via git.

## Implementation sketch

- `llz ci enable-managed-apps --region <env>` (or fold into the existing render/commit): ensure `apps.<name>.enabled: true` for each of `managedApps` (default `[harbor, loki, grafana, kyverno]`) on the apl-core values branch; idempotent (a no-op when already set).
- Wire it into the managed `llz-bootstrap-openbao.yml` path, after apl-core's config repo is reachable, before the converge gate.
- Registry: drop `ManagedConditionalOn` on `harbor`/`observability`/`imageSignature` (they become always-on on managed); keep `imageSignature`'s file under `platform-apl/components/` for cosign-subject-guard.
- Graceful degradation: if the enable-commit fails, warn and keep `imageSignature` gated OFF (don't wedge) — the PR #306 conditional behavior is the safe fallback.

## Open questions

1. **How is managed apl-core pointed at a github.com values branch?** The *content* is known from the pre-refactor SELF-INSTALL path (confirmed in git history): `otomi.git.repoUrl` = the github instance repo, `otomi.git.branch` = a per-env apl-core-OWNED branch `apl-<env>` (NOT main — apl-operator PUSHES its rendered values tree + platform SealedSecrets there every reconcile), `otomi.git.password` = `APL_VALUES_REPO_TOKEN` — set via the helm values `llz render` produced (`RenderValues` / `instance-template/apl-values/values.yaml`). apl-operator then uses that github branch as its **bidirectional** config/values store.
   ADR 0005's managed design deliberately went the OTHER way ("managed apl-core owns its values via apl-api + in-cluster gitea; LLZ does NOT render values.yaml"), and PR #306 removed the values-render accordingly. **So this is a DESIGN CORRECTION, not just a discovery: to get the github model on managed we RESTORE the `otomi.git` → github render for managed.** The one genuinely-open piece is the WRITE PATH to a *Linode-installed* apl-core (self-install used the helm install; managed needs to reconfigure the already-installed apl-core — a one-time gitea→github flip, an apl-api call, or an injectable values overlay apl-operator reads) — confirm against a live managed cluster.

**RESOLVED (BYO Git — techdocs.akamai.com/app-platform/docs/byo-git):** App Platform supports "Bring Your Own Git": the git config lives in the k8s Secret `apl-secrets/apl-git-config` (`otomi.git.{repoUrl,username,password,email,branch}`) and **reloads at runtime without a pod restart**. On a managed cluster you switch via Console→Settings>Git (apl-api-backed), but since it's just that Secret, **LLZ patches `apl-secrets/apl-git-config` directly** with the github coords (repoUrl=instance repo, branch=`apl-<env>`, username=`x-access-token`, password=`APL_VALUES_REPO_TOKEN`). This is the self-install values model restored: seed the `apl-<env>` github branch (restore `ensureAplValuesBranch`) + patch the Secret; apl-operator reloads, switches to github, and installs the apps enabled in the values. So step 1 = RESTORE the `otomi.git`→github + `apps.<default>.enabled` render #306 removed, plus the NEW `apl-git-config` patch.
   Bonus: once apl-core's `otomi.git` → the github instance repo, apl-core's own ArgoCD carries that repo credential, so `platform-bootstrap` can reuse it and the PR #306 `instanceRepoArgoSecretManifest` fix likely becomes redundant (keep as belt-and-suspenders).
2. **Exact values schema** for enabling each app on apl-core v6 (`apps.<name>.enabled` vs a nested/`_rawValues` shape) — verify against apl-core `apps.yaml` / `chart/apl/values.yaml`.
3. **apl-api contract** (Path B fallback): exact method/path/payload from `src/openapi/*.yaml`, and how automation obtains a JWT in-cluster.
4. **Timing** enable→CRDs-ready, and whether the 40×90s Argo retry budget reliably covers a cold apl-operator install of Kyverno/harbor/loki.
5. **Version compat** across apl-core 6.x.
6. **Ownership/drift:** document `managedApps` as source-of-truth; the reconciler re-asserts; operators should not hand-toggle these apps in the Console.

## Consequences

- **Removes** the opt-in gating and the "managed clusters run unverified LLZ images" gap — image-signature is always active because Kyverno is always installed.
- **Adds** a managed bootstrap responsibility (enable the default apps) and a dependency on apl-core reconciling a github values branch.
- The live-convergence e2e (PR #306) should first prove the CORE managed path (minimal core + LLZ extras that need no opt-in app); this ADR's default-apps layer is the next increment on top of that.

Sources: [linode/apl-api](https://github.com/linode/apl-api), [linode/apl-core](https://github.com/linode/apl-core), [apl-core/apps.yaml](https://github.com/linode/apl-core/blob/main/apps.yaml).
