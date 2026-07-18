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
**and PushSecret** bound to the store — what `nudge-argo`'s force-sync half
does from CI, but event-driven, in-cluster, and active on every recovery
(day-2 store blips included, which CI never sees). Same justification the
argo-nudge lane already carries: converting a documented driver from
CI-imperative to watch-triggered is not a new driver (contract anti-pattern
#4/#6 analysis in `reconcile_argo_nudge.go`'s header).

**Lane mechanics** (mirrors the argo-nudge lane's registration in
`reconcile.go`):

- Flag `--reconcile-es-store-recovery`, default off per lane convention,
  enabled in the Deployment args. Leader-gated like every driving lane.
- Watch `clustersecretstores?fieldSelector=metadata.name=openbao` for
  immediacy + a resync floor (300s) as the safety net; RBAC gains
  `list, watch` on `clustersecretstores` (resourceNames `openbao`,
  `openbao-push`) and `get, list, patch` on `externalsecrets` + `pushsecrets`
  cluster-wide — the patch surface is metadata annotations only.
- Transition tracking is in-memory (`lastReady`), so a pod restart
  mid-bootstrap re-bumps once if the store is Ready but any bound
  ExternalSecret still reports not-Ready — idempotent (a redundant bump is
  one cheap ESO reconcile), so restart amnesia is harmless.
- New gauges: `llz_es_store_ready` and `llz_es_recovery_nudges_total`, so the
  e2e can *assert the lane fired* (see rollout) and day-2 alerting can see a
  store that never recovers.
- **PushSecret gap**: the CI force-sync (`nudge-argo`) annotates only
  `externalsecret` — the three PushSecrets (harbor-admin, grafana-admin,
  otel-ingress) were never covered and today ride out their own intervals
  after a store recovery. The lane covers both kinds; this is a net coverage
  gain over the mechanism it replaces, not just a relocation.

**Prerequisite — break the reconciler's own circular dependency.** The
Deployment consumes `linode-api-token` via an env `secretKeyRef`
(`deployment.yaml`), so the pod sits in CreateContainerConfigError until the
store serves — the argo-nudge and store-recovery lanes are offline exactly
when they matter most (this is why the Deployment carries the wave-6
annotation and its inversion war-story). Making the env ref `optional: true`
is NOT sufficient: Kubernetes never injects env into a running pod, so the
linode lanes would stay token-less until a restart — and that same property
means **today's pod serves a stale token after any linode/api-token rotation
until something restarts it**, a live day-2 gap independent of bootstrap.
Fix both at once:

- Replace the env ref with an **optional Secret volume** and a lazy per-tick
  file read (kubelet refreshes mounted Secret content, ~1m): a helper
  `linodeToken()` reads the file, falling back to `LINODE_TOKEN` env for
  CLI/CronJob compatibility. The linode-dependent lanes (volume-labels,
  cidr-firewall, linode-creds) self-gate: token absent → clean no-op + a
  `llz_reconcile_linode_token_present` gauge, never a crash.
- With no hard secret ref the pod is Ready immediately, so the Deployment's
  sync-wave drops from 6 to 0 — the wave-6 workaround (and its comment)
  retires with the inversion it papered over. The `linode-api-token`
  ExternalSecret stays at wave 5 unchanged.

**Not moving**: the lane does NOT revalidate the store itself (the post-seed
CI bump keeps that — CI uniquely knows "seeding just finished"), and Harbor
provisioning stays out of the reconciler (mesh-unreachable from this
namespace, per the Deployment's header note).

### Phase 3 — retire the CI steering (after a validated Phase 2)

Call-site by call-site, with the lane that supersedes each:

| CI call today (`llz-bootstrap-openbao.yml`) | superseded by | disposition |
|---|---|---|
| post-seed `nudge-argo`: app-sync half | argo-nudge lane, live from first boot once the wave-6 inversion is gone | delete |
| post-seed `nudge-argo`: store-Ready wait + ES force-sync half | store-recovery lane fires on the very Ready transition the CI bump triggers | slim to `nudge-argo --store-only` (bump + bounded Ready wait as a converge precondition) |
| Kyverno admit preflight's `nudge-argo \|\| true` | argo-nudge lane re-triggers phase=Failed apps within seconds continuously | delete the nudge call; keep the dry-run admission probe (it produces the actionable warning) |
| redis-realign's `nudge-argo \|\| true` | partially — the lane's transient ComparisonError patterns must first be extended to the WRONGPASS/NOAUTH signature | keep until the pattern lands, then delete |
| `kick-harbor-provisioner` | NOT this design (PostSync provisioner hook is its own) | stays |

Rollout gates, in order:

1. Phase-2 PR lands with the CI nudges **still in place** (belt and
   suspenders); the e2e's prom-metrics step additionally dumps
   `llz_es_recovery_nudges_total` and `llz_reconcile_linode_token_present`.
2. A full release-e2e pass whose converge log shows (a) the store-recovery
   lane fired ≥1, (b) the reconciler pod went Ready *before* the store did
   (wave-0 start proven), (c) the linode lanes activated once the token
   arrived.
3. Phase-3 PR deletes per the table; a second full e2e pass green without
   the CI halves is the merge gate.

## Invariants

1. Every OpenBao-bound ExternalSecret/PushSecret declares
   `refreshInterval ≤ 5m`, or `0` with a generator source (guard-enforced).
2. No new CI verb may force-sync ESO resources or Argo apps once Phase 3
   lands; the reconciler lanes are the sole owners (convergence-contract #6).
3. Application CRs stay health-inert; no `argoproj.io/Application` health
   customization (blast-radius decision stands).

## Rollout

1. This PR: ADR + Phase 1 (interval caps + guard).
2. Phase 2 PR: store-recovery lane + optional-volume token read + wave-6
   retirement, with the CI nudges still in place; gated by the three
   e2e-observable proofs listed under Phase 3's rollout gates.
3. Phase 3 PR: delete/slim the CI call sites per the disposition table; a
   full e2e pass green without the deleted halves is the merge gate.
