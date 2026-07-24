# Design: apl-core-native object storage via a reconciler-driven `apl-overlay`

**Status:** In progress — code landing on branch `fix/loki-memory-3g` (continuation
of the instance-diet / in-cluster-credential series). Runtime behaviour (does
apl-operator consume the overlaid files) is **lab-gated** — see Lab-validation.

**Date:** 2026-07-23

**Relates to:** [apl-core-v6-migration.md](apl-core-v6-migration.md) (the
"declined for now" decision this reverses), [apl-core-values-branch-isolation.md](apl-core-values-branch-isolation.md)
(the `apl-<env>` machine-owned branch this writes to), [kube-native-reconciler.md](kube-native-reconciler.md)
(the reconciler this adds a pass to), [linode-credential-rotator.md](linode-credential-rotator.md)
(the in-cluster key rotation this now feeds).

## Context — why this reverses a declined decision

[apl-core-v6-migration.md](apl-core-v6-migration.md) §"apl-core-native object
storage (`obj.provider.linode`)" **evaluated and declined** adopting apl-core's
native S3 wiring, for two reasons:

1. It "requires static S3 credentials in apl-core's values (sealed, rotated only
   by values edits), which conflicts with the landing zone's in-cluster
   key-rotation model (keys minted at bootstrap, rotated without touching git)."
2. "apl-core's model is one-bucket-per-app vs the landing zone's three Loki
   buckets (chunks/ruler/admin)."

The doc closed with: **"Revisit if the rotation model changes or apl-core learns
to source obj creds from a Secret."** This design revisits objection #1 head-on:
instead of routing obj creds through a Secret, it keeps the in-cluster rotation
model AND feeds apl-core's native values — by making a reconciler the thing that
does the "values edit" on every rotation. The credential is *not* static; it is
re-derived from OpenBao and git-synced continuously. That collapses the conflict:
apl-core gets its native `obj.provider.linode` values, and the landing zone keeps
minting/rotating the key in-cluster with no human values edit.

Objection #2 (one Loki bucket vs three) is accepted, not worked around: native
mode uses apl-core's single `buckets.loki`. Consolidating the Loki store from
three buckets to one is a **lab-gated migration item** (see below); this change
ships the overlay + reconciler plumbing that makes native obj possible, gated on
that validation before any env flips its live Loki/Harbor S3 wiring to consume it.

## The two pieces

### 1. `llz render` emits `apl-values/{_shared,<env>}/apl-overlay/`

`llz render` gains a third committed apl-values artifact class (alongside the
manifest kustomizations and `values.yaml`): a small **overlay tree** that is the
*source of truth* for the apl-core config the landing zone owns.

- `_shared/apl-overlay/obj.yaml` — the instance-wide skeleton of the apl-core
  `obj:` block: `showWizard: false`, `provider.type: linode`, and the
  `accessKeyId` / `secretAccessKey` **placeholders** (`${obj_access_key_id}` /
  `${obj_secret_access_key}`) the reconciler fills. No secret material is
  committed.
- `<env>/apl-overlay/obj.yaml` — the per-env override: `provider.linode.region`
  (the object-storage cluster id) and `provider.linode.buckets` (`loki`,
  `harbor`), derived from the spec exactly as `objectStoreWiring` derives the
  existing bucket names.
- `_shared/apl-overlay/apps.yaml` + `<env>/apl-overlay/apps.yaml` — the **AplApp
  toggles**: `apps.<name>.enabled` for the apl-core apps the deployment's
  components turn on/off (the same truth `RenderValues` writes into
  `values.yaml`, expressed as an overlayable fragment). `_shared` carries the
  instance-wide defaults; `<env>` overrides per deployment.

The overlay is rendered by the spec, drift-guarded by `llz render --check`, and
committed on `main` (human-authored source), never carrying secrets.

### 2. The `apl-overlay` reconciler — git-to-git sync (replaces the force-push)

A new `--reconcile-apl-overlay` pass in the in-cluster reconciler
([reconcile.go](../../tools/cmd/llz/reconcile.go)) does a **git-to-git overlay
sync**, replacing the config role a force-push previously played:

1. **Read** the overlay from the primary repo (`main`) over the GitHub REST
   git-data API — the reconciler runs on the distroless image with no `git`
   binary and no shell, so it uses `net/http`, not `exec("git")`, consistent with
   the reconciler's no-client-go stance.
2. **Fill** `accessKeyId` / `secretAccessKey` from OpenBao `secret/obj/platform`
   (read via the `reconciler` Kubernetes-auth role; grant added to
   `policyReconcilerRead`). This is the step that keeps the credential live: the
   linode-creds reconciler rotates the key into OpenBao, and this pass propagates
   it into apl-core's values without a human edit.
3. **Merge** `_shared` + `<env>` (a deep key-level merge, env wins).
4. **Overlay** *only the owned files* onto the machine-owned `apl-<env>` branch
   (see [apl-core-values-branch-isolation.md](apl-core-values-branch-isolation.md))
   using a GitHub tree with `base_tree` = the branch's current tree, so every
   file the landing zone does NOT own (apl-operator's `env/` tree, its
   SealedSecrets) is preserved untouched.
5. **Commit + push with fast-forward retry** — a non-force ref update. If
   apl-operator pushed concurrently (a non-fast-forward 422), the pass re-reads
   the head and rebuilds, up to a small attempt budget. This is the cooperative
   replacement for a force-push: two writers share `apl-<env>` without either
   clobbering the other. A no-op when the branch already matches the desired
   overlay (the tree sha is unchanged) — so it is quiet in steady state.

### Owned-file mapping (the one lab-gated assumption)

The reconciler maps each overlay file to a path in the `apl-<env>` values tree
that apl-core reads as config input. The exact target paths — and whether any
target needs a key-level (not file-level) merge because apl-operator co-writes
the same file — are **apl-core-internal and provable only on a live cluster**,
the same disposition every other apl-core-facing assumption in this repo carries
(see the v6-migration lab checklist). The mapping is isolated in one documented
table (`aplOverlayTargets`) so a lab finding is a one-line correction, and the
overlay files are kept minimal (obj block only; the enabled map only) to shrink
the blast radius of a co-written file.

## Credential consolidation — `secret/obj/platform`

Native obj uses **one** provider credential (`accessKeyId` + `secretAccessKey`)
across all buckets, replacing the two per-app keys the landing zone mints today
(`secret/loki/object-store`, `secret/harbor/registry-s3`). This change adds the
consolidated `secret/obj/platform` key to the in-cluster rotation table (one
Linode object-storage key spanning the loki + harbor buckets, `read_write`) and
grants the `reconciler` role read on it. The two per-app keys stay for now (Loki
and Harbor still consume their existing ESO Secrets until they are flipped to
native obj in the lab-gated migration); `secret/obj/platform` is the forward
credential the overlay feeds.

## Lab-validation checklist (gate before any env consumes native obj)

- [ ] apl-operator **re-reads** the overlaid config files on `apl-<env>` and the
      merged `obj.provider.linode` + `apps.*.enabled` take effect (the owned-file
      mapping is correct; no key-level clobber of an operator-co-written file).
- [ ] The **ff-retry** genuinely converges against apl-operator's concurrent
      reconcile pushes (observe the reconciler's non-fast-forward retries on a
      live `apl-<env>` and confirm no force-push and no lost operator commits).
- [ ] The reconciler's OpenBao read of `secret/obj/platform` authenticates under
      the `reconciler` role and the filled `accessKeyId`/`secretAccessKey` reach
      apl-core.
- [ ] **Single Loki bucket.** Before flipping Loki to native obj, the
      object-storage module provisions the consolidated `platform-loki-<env>`
      bucket (or Loki multiplexes chunks/ruler/admin as prefixes within it), and
      Loki reads/writes it healthily. Until then Loki stays on its three-bucket
      `_rawValues` wiring.
- [ ] `main` stays free of the filled credential (the overlay on `main` keeps the
      `${...}` placeholders; only the machine-owned `apl-<env>` branch carries the
      resolved value).

## Alternatives considered

- **Keep the declined status quo** (three-bucket `_rawValues` + kyverno S3 flip).
  Rejected as the forward direction — it keeps the S3 wiring in `_rawValues`
  duplicated per app and never lets apl-core own its own storage config; the
  reconciler sync removes the objection that blocked native obj.
- **Force-push the config onto `apl-<env>`** (the role this replaces). Rejected —
  a force-push clobbers apl-operator's `env/` tree and its SealedSecrets on the
  shared branch; the whole branch-isolation model exists to let the operator own
  that tree. Cooperative ff-retry overlay is the non-destructive equivalent.
- **Source obj creds from a Kubernetes Secret** (apl-core's own future option
  noted in the v6 doc). Not available on 6.0.0 (`secretAccessKey` is an inline
  `x-secret` in values, not a `secretKeyRef`); the git-sync is the 6.0.0-native
  way to keep it live. Revisit if apl-core learns a `secretKeyRef` for obj.
