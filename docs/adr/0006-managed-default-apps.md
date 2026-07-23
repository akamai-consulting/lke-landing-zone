# ADR 0006 — Managed default apps: enable apl-core's harbor/loki/grafana/kyverno via the values branch; layer LLZ extras on the managed installs

Status: **Accepted & e2e-VALIDATED** (PR #306, run 29982807364, 2026-07-23). The first delivery (a chart-driven `helm upgrade`) was **e2e-disproven** — a managed cluster has no customer-`helm`-upgradeable `apl` release (`"apl" has no deployed releases`), and app-enablement does not flow through chart values post-install. Replaced with the supported mechanism — **BYO-Git in-cluster values-repo migration + Secret repoint** — which is now proven end-to-end (apps install, Kyverno CRD lands, imageSignature applies; see Live-validation status). A separate downstream managed-convergence issue (CoreDNS/istio under full app load) remains, tracked below.
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
3. **Keep the per-app gating, but couple it to `managedApps` (which is now default-on), not to operator opt-in.** The `ManagedConditionalOn` gate stays — the `harbor`/`observability`/`imageSignature` components emit IFF their app is in `managedApps` — but `managedApps` now defaults to `[harbor, loki, grafana, kyverno]`, so a default spec emits all three extras. The gate is no longer "wait for a human to opt in in the Console"; it's "emit the extra exactly when LLZ enabled the underlying app." This is strictly safer than dropping the gate outright: an operator who NARROWS `managedApps` (e.g. drops kyverno) automatically suppresses the matching extra (imageSignature) instead of wedging converge on a missing CRD. `imageSignature` (the Kyverno ClusterPolicy) is safe in the default case because Kyverno's CRD is guaranteed present. The gate and the `configureManagedApl` app-enable read the SAME list, so they can never disagree.
4. **Ordering is absorbed by Argo.** LLZ commits `apps.*.enabled` early; apl-operator installs the apps (CRDs land) over the following minutes; the LLZ extras that reference those CRDs are `SkipDryRunOnMissingResource=true` with the load-bearing retry budget (40 @ 90s), so they converge once the CRDs appear rather than hard-failing.

`spec.cluster.bootstrap.managedApps` is retained as the **declarative set of apps LLZ enables** (defaulting to `[harbor, loki, grafana, kyverno]`), no longer an operator opt-in — LLZ reconciles apl-core to it, and the `llz-reconciler` can re-assert it against Console drift.

## Mechanism: repoint git + write `apps.<name>.enabled` (corrected)

apl-core's config is Git-as-database (apl-api/Console commits to it; apl-operator reconciles from it). Write-paths evaluated:

| Path | How | Verdict |
|---|---|---|
| ~~**helm upgrade --reuse-values --set otomi.git.\*,apps.\*.enabled**~~ | Re-point + enable apps by upgrading the installed `apl` chart. | **REJECTED — e2e-disproven.** A managed (`apl_enabled`) cluster has **no customer-`helm`-visible `apl` release** (`Error: UPGRADE FAILED: "apl" has no deployed releases`; managed upgrades are a Console button). And per apl-core source, chart values (`VALUES_INPUT`) are read **only at first-install bootstrap** — post-install the operator reconciles app state **exclusively from the git values repo**, so `--set apps.*.enabled` is a no-op even where a release exists. |
| **A — BYO-Git Secret patch + values-repo migration** (CHOSEN) | Patch `apl-secrets/apl-git-config` (bare keys) to repoint apl-core at the github `apl-<env>` branch, having first pushed apl-core's current values tree there **with** an `env/apps/<name>.yaml` enable file per app. | Supported (BYO Git; the Secret is re-read every reconcile → runtime `git remote set-url`, no restart). App-enablement lands as real values-repo state the operator honors. No helm, no apl-api JWT. |
| **B — drive apl-api (the Console's path)** | `PUT`/`PATCH` the settings/apps endpoint on `apl-api` (`otomi` ns). | JWT-authenticated (Keycloak login); the JWT is the automation blocker. Fallback only. |

**Why the migration (not just a Secret patch):** the operator does `git reset --hard origin/<branch>` on every poll (`git-repository.ts`). Repointing at a branch that held **only** app-toggle files (or an empty/partial tree) would **wipe** apl-core's tracked config. Wipe-safety is exact (source-verified): a *non-existent* target branch → `fetch` fails *before* the reset → ENV_DIR intact, operator retries; an *existing-but-partial* branch → reset deletes the missing tracked files. So LLZ force-pushes the **complete** tree (current tree + enable files) to the github branch **before** flipping the Secret — the same "push existing values history" the Console's BYO-Git wizard performs.

**Why in-cluster (a Job, not runner-side git):** `apl-git-config.repoUrl` on managed is an **in-cluster-only** Service DNS (`http://git-server.git-server.svc.cluster.local/otomi/values.git`; `git-server` ships `httproute.enabled: false` — no public route). A CI runner with only kube-API access cannot reach it (e2e-confirmed: `Could not resolve host`). The operator pod's ENV_DIR tree is **SOPS-decrypted** in place, so `kubectl cp`-ing it out would leak secrets and mismatch the encrypted-at-rest format — the encrypted remote must be cloned. LLZ therefore runs the clone→enable→push as an in-cluster **Job** (`alpine/git`) whose creds come from a short-lived Secret; the runner-side step is only the final `kubectl patch` of `apl-git-config`.

**App-enable file shape (source-verified against apl-core):** `env/apps/<name>.yaml`, keyed off the **filename**, contents:
```yaml
kind: AplApp
metadata:
  name: <name>
spec:
  enabled: true
```
`loadValues()` globs `env/apps/*.yaml` into the aggregate `values-repo.yaml`; `spec` becomes `apps.<name>`. Schema-valid for the default set (harbor has no app-level `required`; loki's `adminPassword` is an `x-secret`, stripped from `required` before validation). The operator's write-back preserves `enabled: true`.

## Implementation (corrected, in #306)

- **`configureManagedApl(o, d)`** in `llz ci bootstrap-cluster` (best-effort, after `waitManagedArgoReady`):
  1. `readAplGitConfig` — read the current `apl-secrets/apl-git-config` Secret (base64 `.data`, **bare** keys) for apl-core's present (Gitea) coordinates.
  2. `migrateAplValuesToGitHub` — apply an in-cluster **Job** (`alpine/git`, creds via a short-lived Secret) that clones that (in-cluster, encrypted) tree, writes `env/apps/<name>.yaml` for each `managedApp`, and **force-pushes the whole tree** to the github `apl-<env>` branch; poll it to completion.
  3. `patchAplGitConfig` — merge-patch `apl-secrets/apl-git-config` `stringData` → `{repoUrl: github, branch: apl-<env>, username: x-access-token, password: <token>}` (bare keys, **`username` not `user`** — the `otomi.git.user` values-comment is a doc bug; every consumer uses `username`). The operator reloads it next poll, `git remote set-url`s to github, hard-resets to our complete+toggled tree, and installs the apps.
- The app list is `o.managedApps`, populated in `runBootstrapCluster` from `spec.cluster.bootstrap.managedApps` (which `Defaults()` fills to `clusterspec.DefaultManagedApps = [harbor, loki, grafana, kyverno]`), fallback to `DefaultManagedApps`.
- Registry: `ManagedConditionalOn` on `harbor`/`observability`/`imageSignature` reads `managedApps`; with the default set all three emit. `imageSignature`'s file stays under `platform-apl/components/imageSignature/` for cosign-subject-guard.
- `verify.go` reads the BYO-Git repoUrl from the **Secret** `apl-secrets/apl-git-config` (base64), NOT a ConfigMap in `apl-operator` (that earlier check was a wrong-kind/wrong-namespace no-op).
- Graceful degradation: best-effort (warn, don't fail the bootstrap). A failure leaves apl-core on Gitea with the default minimal apps; the render-side gate still emitted `imageSignature` (kyverno declared), so this is the one path that can still wedge — mitigated by the Argo `SkipDryRunOnMissingResource` + retry budget once the app-enable eventually succeeds.

**Live-validation status: VALIDATED (e2e run 29982807364, 2026-07-23).** The full chain works end-to-end: the in-cluster `alpine/git` Job waits for apl-core's values branch, clones the encrypted git-server tree, orphan-commits the full tree + the `env/apps/<app>.yaml` toggles, force-pushes to the github `apl-<env>` branch, and LLZ patches `apl-git-config` to repoint apl-core there. Bootstrap logged `✓ managed apl-core repointed at … + apps enabled (harbor, loki, grafana, kyverno)` and — the payoff — `clusterpolicy.kyverno.io/verify-llz-image-signature serverside-applied`: **Kyverno installed and the imageSignature ClusterPolicy applied; the "no matches for kind ClusterPolicy" wedge is gone.** Fixes it took (all e2e-driven): in-cluster Job (repoUrl is `git-server.git-server.svc`, http, no public route); embed http creds; percent-encode the userinfo (generated password has `&`); wait for apl-core to push its branch (race); push an orphan commit (shallow-clone push sends an incomplete pack).

**Known follow-on (NOT this ADR's mechanism — a separate managed-convergence problem):** once the wedge clears, converge exposes a downstream failure — the fresh cluster's CoreDNS times out under the simultaneous harbor+loki+grafana+kyverno install (istio sidecars can't reach istiod → cert cascade; configmap-mount timeouts), plus a `validate-orcs-registry-cluster` Kyverno compliance PolicyViolation. Likely needs node-pool sizing and/or staged app-enable and/or scoping the compliance policy — tracked separately.

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
