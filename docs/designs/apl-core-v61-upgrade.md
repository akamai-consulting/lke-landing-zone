# Design: apl-core 6.0.0 → 6.1.0 upgrade

**Status:** Partial — baseline moved on branch `feat/apl-6.1.0-upgrade`; pinned to the GA
`v6.1.0` release (published 2026-07-28). Validate in lab before any non-lab
promotion.
**Relates to:** [apl-core-migration-runbook.md](../apl-core-migration-runbook.md),
[apl-core-v6-migration.md](apl-core-v6-migration.md) (the 5.x → 6.x predecessor),
[apl-overlay-obj-native.md](apl-overlay-obj-native.md),
[../adr/0005-managed-app-platform.md](../adr/0005-managed-app-platform.md),
`tools/internal/shared/clusterspec/aplversion.go`,
`tools/cmd/llz/ci_prepare_apl_upgrade.go`.

## Why

apl-core `v6.1.0` GA'd on 2026-07-28. Unlike 5.x → 6.x this is a **minor**: it
modernises platform components and reworks secrets handling, but changes none of
the four contracts the landing zone actually depends on. The work is therefore a
re-pin plus one operational prerequisite — not a migration.

The value of this document is the **negative** result: recording *which* contracts
were checked and found unchanged, so the next bump does not have to re-derive it,
and so the "verify in lab" list is short and specific rather than "re-test
everything".

## What the landing zone depends on, and what 6.1.0 did to it

Each row was checked against the `v6.0.0…v6.1.0` source diff, not inferred from
the release notes.

| Contract LLZ depends on | Verified at v6.1.0 | Verdict |
|---|---|---|
| **apl-values env-tree paths** — `env/apps/<name>.yaml` (`AplApp`), `env/settings/obj.yaml` (`AplObjectStorage`), `env/teams/<name>/` | `src/common/repo.ts` `getFileMaps()` / `getFilePath()` unchanged; `tests/fixtures/env/` moved only `settings/cluster.yaml` + `settings/otomi.yaml` | **unchanged** — the apl-overlay reconciler needs no change |
| **BYO-Git Secret** `apl-secrets/apl-git-config` with `repoUrl`/`branch`/`username`/`password` | `chart/apl/templates/02-git-secret.yaml` — same name, namespace and keys (the file was renamed within the range, not the resource) | **unchanged** — `configureManagedApl`'s read + merge-patch still applies |
| **ESO API versions LLZ writes** — `external-secrets.io/v1` (ExternalSecret/SecretStore/ClusterSecretStore) and `v1alpha1` (PushSecret) | ESO 2.4.1 → 2.7.0; `v1` is served+storage, `v1beta1` is served:false (already was), PushSecret stays `v1alpha1` | **unchanged** |
| **PushSecret reconciliation** (the eso-pusher path) | `values/external-secrets/external-secrets.gotmpl` dropped its explicit `processPushSecret: true` — but `true` is the chart default in 2.7.0, so the override was redundant | **unchanged** — reads as a break in the diff, is not one |
| **Object-storage credential handoff** — LLZ delivers `apl-secrets/obj-secrets`, apl-core derives `loki-s3-linode-credentials` | `values/loki/loki.gotmpl` `$s3SecretName` unchanged; the only edit is `gateway.metrics.enabled: false` | **unchanged** |
| **apl chart values schema** (`llz ci validate-apl-values` runs `helm template apl/apl`) | published `values.schema.json` byte-identical between `6.0.0` and `v6.1.0` | **unchanged** |

## What DID change, and what we did about it

### 1. The published chart version gained a `v` — and it is load-bearing

apl-core's release automation was reworked for this cycle (upstream ADR
`2026-06-02-release-branch-per-cycle`). A side effect: the published Chart.yaml
went from `version: 6.0.0` to `version: v6.1.0`, and the chart git tag from
`apl-6.0.0` to `apl-v6.1.0`.

```
$ helm search repo apl/apl --versions
apl/apl   v6.1.0   v6.1.0
apl/apl   6.0.0    v6.0.0
```

`helm --version 6.1.0` still resolves — helm reads the flag as a semver
constraint and `v6.1.0` normalises to `6.1.0` — but only via a fallback:

```
level=WARN msg="unable to find exact version requested" chart=apl requested=6.1.0 selected=v6.1.0
```

So `BaselineAplChartVersion` carries the exact published string, prefix and all,
and a test asserts the prefix survives the next bump. Every comparison in the
landing zone (`aplSemver`, `semver`) already strips a leading `v`, so an existing
spec pinned to a bare `6.1.0` is still `DriftNone` — no instance has to change.

### 2. The pre-upgrade annotation (the one real action item)

The release notes require, **before** the upgrade rolls:

```sh
kubectl annotate deployment apl-operator \
  argocd.argoproj.io/sync-options=Force=true,Replace=true -n apl-operator
```

6.1.0 ships that annotation in its own chart
(`values/apl-operator/apl-operator.gotmpl` gained `deploymentAnnotations`), but
that value only exists once 6.1.0 has synced — and the sync is what needs it.
Without the annotation Argo CD attempts a plain apply over the 6.0.0 Deployment
whose fields the 6.1.0 spec rewrites; the sync fails and the operator is left
mid-upgrade.

On the managed App Platform Linode owns the apl-core version *and picks when it
rolls* (ADR 0005), so the landing zone has no upgrade hook to hang this on and an
operator cannot reliably win the race by hand. `llz ci bootstrap-cluster`
therefore asserts it **eagerly, on every apply**, and `llz ci
prepare-apl-upgrade` exposes it standalone for a cluster that is not being
bootstrapped. It is idempotent and self-retires once the fleet is on 6.1.x.

### 3. Component bumps worth knowing about

| Component | 6.0.0 → 6.1.0 | Why it matters here |
|---|---|---|
| **Loki chart** | `6.55.0` → `17.4.11` | Not a 10-major jump — apl-core switched from the grafana-maintained chart to the **grafana-community** chart, which renumbers. appVersion moves `3.6.7` → `3.7.2` and the `grafana-agent-operator` subchart is dropped. The values contract LLZ touches is unchanged, but this is the single biggest in-place change in the release: **verify Loki ingest + the object-store secret in lab first**. |
| **Istio** | `1.29.2` → `1.30.2` | apl-core added a runtime upgrade (`runtimeUpgrades` v6.0.1) that restarts outdated sidecars after istiod upgrades. LLZ's PSS-`restricted`-vs-sidecar carve-outs in `llz-cluster-foundation` are the thing to watch. Ambient still defaults off — do not enable it here. |
| **sealed-secrets** | `2.18.5` → `2.18.6`, and `keyrenewperiod: 0` | Automatic sealing-key renewal is now **disabled** so admins can back keys up during a maintenance window. This removes a moving part behind the sealed-secrets key-mismatch failure mode, but makes **backing the key up an explicit operator responsibility**. |
| **Argo CD** | `9.5.17` → `9.7.1` | Includes upstream "create argocd redis secret as needed" and a tightened argocd-controller restart condition — both in the area of the repo-server↔redis `WRONGPASS` split LLZ self-heals in `health.go`. **Keep the self-heal**: it is cheap, and whether these fix our specific re-seed path is unproven on a live cluster. Re-evaluate after a lab run. |
| **external-secrets** | `2.4.1` → `2.7.0` | New CRDs (`ClusterGenerator`, BeyondTrust dynamic secret) that LLZ does not use; the versions it does use are unchanged. |
| **kube-prometheus-stack** | `85.4.0` → `86.3.2` | `monitoring.coreos.com/v1` unchanged — LLZ's ServiceMonitors/PrometheusRules are unaffected. |
| **console / api** | `v5.0.3` → `v5.1.0` | Carries apl-console PR #814, which gates the onboarding "Configure Git Repository" modal on `isDefaultGitConfiguration`. That modal auto-opening on a *correctly* BYO-Git-configured managed cluster was a known, purely cosmetic annoyance; **this upgrade resolves it** and the "dismiss it once" workaround can be dropped. |
| **apl-operator intervals** | new `operator.gitOpTimeoutMs` / `installRetries` / `installMaxTimeoutMs`; `pollIntervalMs` default `1000` → `30000` | All defaulted in-chart. LLZ overrides none of them, but a 30× slower git poll changes how fast an apl-overlay push is picked up — factor it into convergence-timing expectations rather than treating a slower first reconcile as a regression. |

## Support floor vs. target

The floor and the target now differ deliberately for the first time:

- `clusterspec.BaselineAplChartVersion` = `v6.1.0` — what this release **targets**.
- `minSupportedAplChartVersion` = `6.0.0` — the oldest chart still **supported**.

Nothing in the table above makes the landing zone 6.1-only, so raising the floor
in lockstep would hard-fail every 6.0.0 instance for no reason. A 6.0.0 pin is
minor drift: `llz` warns (`AplChartVersionWarnings`) and proceeds. Raise the
floor only when a 6.0.0 cluster genuinely stops working, and record why in
`ci_assert_apl_version.go` the way the 5.x rationale is recorded there.

## Lab checklist before promoting past lab

1. `llz ci prepare-apl-upgrade` reports the annotation applied, **then** let the
   managed upgrade roll. Confirm `apl-operator` is Synced, not stuck mid-upgrade.
2. Loki: logs still ingesting; `loki-s3-linode-credentials` present and the
   object-store buckets still being written (the community-chart switch is the
   highest-risk item).
3. Istio: no pods left on the old sidecar image; the `llz-cluster-foundation`
   NetworkPolicies still admit platform traffic.
4. Sealed-secrets: capture a backup of the sealing key now that renewal is off.
5. Argo CD: watch for a repo-server↔redis `WRONGPASS` flap across the upgrade —
   if `health.go`'s self-heal never fires over several converges, that is the
   evidence needed to consider retiring it.
