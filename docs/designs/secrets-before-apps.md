# Secrets-before-apps: bounded secret propagation without app-of-apps serialization

## Problem

Four separate mechanisms exist to push the platform past the same underlying
gap — that OpenBao-backed secrets become available *mid-sync*, after the
consumers that need them have already been applied:

| mechanism | where | what it compensates for |
|---|---|---|
| `llz ci nudge-argo` (app half) | CI, post-seed + pre-converge | Argo CD does not retry a terminally-failed sync to the same revision |
| `llz ci nudge-argo` (force-sync half) | CI | ESO does not re-trigger ExternalSecrets when their store recovers |
| argo-nudge reconciler lane | in-cluster (`llz-reconciler`) | same as the app half, day-2 |
| `llz ci kick-harbor-provisioner` | CI, pre-converge | the provisioner's first *useful* tick is cron-paced (separate design: PostSync hook) |

The CI halves violate the spirit of convergence-contract anti-pattern #6 (no
CI-imperative force-sync) and each carries its own timing knob. The measured
cost on passing e2e runs: the post-seed nudge alone ranged 3s–118s, and before
the nudges existed the recovery gap was observed at ~14 minutes of
CreateContainerConfigError — which matches ESO's *error-backoff* ceiling (a
never-synced ExternalSecret requeues with exponential backoff, capping near
~16m), not any refreshInterval we configure.

## Why NOT the obvious fix (serialize apps behind the store)

The tempting root fix is app-of-apps wave gating: configure a health
customization for `argoproj.io/Application` and let the platform-bootstrap
root's waves hold secret-consuming child Apps until `llz-openbao` /
`llz-secret-store` report Healthy. Rejected, for three standing reasons:

1. **Health-inert child Apps are a deliberate design decision**
   ([blast-radius-decomposition](blast-radius-decomposition.md)): a Degraded
   resource fails only its own App. Adding Application health globally re-couples
   every carved App's fate to the parent's sync.
2. **An in-flight sync pins its revision** (the PR #142 scar,
   [argo wave-gating lessons]): gating waves on OpenBao health would hold the
   root's sync operation open for the entire init/unseal/configure window
   (minutes, driven by the *workflow*, outside the cluster's control), during
   which pushes and refreshes queue behind the pinned revision.
3. **It serializes work that today overlaps.** Consumers currently apply early
   and wait benignly (the health.lua entries in `_shared/values.yaml` grade a
   not-Ready store/ExternalSecret/PushSecret as Progressing, and a pod waiting
   on a missing Secret self-heals). Holding their creation until the store is
   Ready would push image pulls, PVC binds, and DB bootstraps *after* the
   OpenBao window instead of alongside it — a net e2e slowdown.

The system's existing posture — apply everything, let secret consumers wait
benignly, health-grade the waiting as Progressing — is correct. What is
*missing* is a bound on how long "waiting" lasts once the store becomes
serviceable, and a kube-native owner for the two retry gaps (Argo terminal
syncs, ESO store recovery) so CI stops steering.

## Design: three phases

### Phase 1 — bound steady-state propagation (this PR)

Every ExternalSecret/PushSecret bound to the `openbao`/`openbao-push` stores
must declare `refreshInterval` ≤ **5m** (or `0` for one-shot generator
ExternalSecrets, which must never re-run — regenerating `grafana/admin` hourly
would rotate a live password). Today four sit at `1h`:

- `components/harbor/harbor-admin-push.yaml` (PushSecret)
- `components/harbor/harbor-robot-provisioner/externalsecret.yaml`
- `components/cidrFirewall/llz-cidr-firewall/externalsecret.yaml`
- `components/broadPatRotator/broad-pat-rotator/externalsecret.yaml` (gh-token)

A 1h interval means a rotated credential (linode PAT, dispatch token) can be
served stale for up to an hour after the rotator writes the new value — an
operational exposure independent of bootstrap. 5m bounds both rotation
propagation and post-blip staleness at ~10 reads/hour against an in-cluster KV
— negligible load.

**Guard**: `llz ci externalsecret-paths` (already in the Makefile's
`externalsecret-paths-check`) now also fails on any OpenBao-bound
ExternalSecret/PushSecret in the platform trees whose `refreshInterval` is
missing or above the cap, so the invariant can't erode.

**Honest scope note**: Phase 1 does NOT close the bootstrap first-sync gap. A
never-synced ExternalSecret is governed by ESO's error backoff, not
refreshInterval; refreshInterval only applies after the first successful sync.
The bootstrap gap is Phase 2's job.

### Phase 2 — a store-recovery watch lane in llz-reconciler (next)

Add a lane to the existing watch-driven reconciler
([kube-native-reconciler](kube-native-reconciler.md)) that watches the
`openbao` ClusterSecretStore's Ready condition and, on a False→True
transition, bumps the `force-sync` annotation once on every ExternalSecret
bound to that store — exactly what `nudge-argo`'s force-sync half does from
CI, but event-driven, in-cluster, and active on every recovery (day-2 store
blips included, which CI never sees). This is the same justification the
argo-nudge lane already carries: a controller *observing* a condition upstream
refuses to watch (contract anti-pattern #4 is about CI driving what a
reconciler should observe — this moves the driving INTO a reconciler).

Prerequisite: the reconciler must run *before* the store is Ready, and today
its Deployment consumes the `linode-api-token` ExternalSecret (wave 6, itself
store-bound) — a circular dependency that keeps the argo-nudge and
store-recovery lanes offline exactly when they matter most. Fix: make the
linode token Secret an **optional** mount/read; lanes that need Linode
self-gate on its presence, the argo-nudge + store-watch lanes need only the
kube API. The reconciler then starts at wave 6 with or without secrets.

### Phase 3 — retire the CI steering (after a validated Phase 2)

With Phase 2 live from first boot:

- `nudge-argo`'s force-sync half → delete (the store-watch lane owns it).
- `nudge-argo`'s app-sync half → delete (the argo-nudge lane now runs from
  first boot; the bootstrap keeps only the one post-seed ClusterSecretStore
  revalidation bump, which is knowledge CI uniquely has — "seeding just
  finished, validate NOW").
- The Kyverno admit preflight and `kick-harbor-provisioner` are separate
  designs (signature-ready-at-publish; PostSync provisioner hook) and are not
  retired by this one.

Each deletion lands only after a full release-e2e pass with the corresponding
in-cluster lane proven in the run's converge log.

## Invariants

1. Every OpenBao-bound ExternalSecret/PushSecret declares
   `refreshInterval ≤ 5m`, or `0` with a generator source (guard-enforced).
2. No new CI verb may force-sync ESO resources or Argo apps once Phase 3
   lands; the reconciler lanes are the sole owners (convergence-contract #6).
3. Application CRs stay health-inert; no `argoproj.io/Application` health
   customization (blast-radius decision stands).

## Rollout

1. This PR: ADR + Phase 1 (interval caps + guard).
2. Phase 2 PR: reconciler store-watch lane + optional linode mount; validated
   by a release-e2e run whose converge log shows the lane firing.
3. Phase 3 PR: delete the CI force-sync/nudge halves; e2e green without them.
