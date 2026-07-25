# ADR 0002 — Reframe LLZ as the APL CLI: one binary, two altitudes

- Status: Proposed
- Date: 2026-07-24
- Deciders: platform / LLZ maintainers
- Related: `docs/designs/apl-core-values-branch-isolation.md`,
  `tools/cmd/llz/reconcile_apl_overlay.go`,
  `docs/adr/0001-pat-rotation-locus.md`

## Context

`llz` began life as a **landing-zone** provisioner: its unit of work is a Linode
LKE cluster (or a fleet of them), and Akamai App Platform (APL / otomi-core) is
one of the payloads it installs onto that substrate. The center of gravity is
Linode/LKE + the account-level fleet; APL is downstream of provisioning.

Several recent efforts have quietly inverted that relationship:

- `llz render` now owns the full APL `values.yaml` from a typed `LandingZone`
  spec, and `--reconcile-apl-overlay` writes onto the `apl-<env>` git tree over
  the GitHub git-data API (distroless, no git binary, ff-retry merge) — a more
  principled path than apl-operator's in-cluster `otomi commit`.
- The managed-APL pivot made `llz` target a **running** APL control plane it did
  not install (`discoverManagedDomain`, `discoverKeycloakIssuerFromCluster`,
  managed-aware component filtering in `llz render`).
- `spec.teams` / managed-team-provisioning and the two-backend `spec.components`
  model pushed the spec toward APL's own domain nouns (teams, apps, values).

Meanwhile, APL itself has **no first-class, operator-facing CLI**. The historical
`otomi` command (`bootstrap`/`apply`/`commit`/`validate-values`) is container-bound
(Docker + Bash 4+) and has receded behind the Console + `otomi-api` GitOps loop;
it is the platform's internal automation engine, not a front door. There is a gap
where a typed, GitOps-native, operator-facing `apl` CLI would sit — and `llz` is
already most of the way into it.

The risk of doing nothing is that the inversion stays implicit: APL-domain
concepts and Linode/fleet concepts remain tangled in one flat command and spec
surface, so neither reads as coherent to an APL operator or to an
infra/fleet operator.

## Decision

**Reframe `llz` as the APL CLI using a "one binary, two altitudes" model,
converging on APL's vocabulary where the concept is APL-domain and deliberately
keeping LLZ's variations where the concept is Linode/fleet-specific or where LLZ's
implementation is stronger than vanilla otomi.**

Two decisions settle the shape:

1. **Topology — same binary, `apl` subtree.** Add `llz apl …` command groups now;
   a bare `apl` alias/symlink may follow. No packaging split, no early product
   fork; the APL-layer / provider-layer boundary is allowed to harden in code
   first. (Rejected: a separate `apl` binary — doubles release/CI surface and
   forces the boundary before it is understood; a full `llz`→`apl` rename —
   maximal churn for existing workflows.)

2. **Schema — track upstream, namespace extras.** The APL portion of the spec
   mirrors otomi's `values` schema 1:1 (`clusters[]`, `teams[]`, `apps[]`,
   `policies`); LLZ-only fields are quarantined under `spec.landingZone.*`.
   (Rejected: forking the schema — full control but perpetual drift-reconciliation
   against every upstream otomi values change.)

### The align vs. keep-variation rule

Every subcommand, spec field, and code path is classified by one test:

- **Align to APL** when the concept lives in APL's domain — teams, apps, values,
  secrets/identity, platform health. Map 1:1 to otomi's schema and vocabulary.
- **Keep the variation** when the concept is (a) Linode/fleet-specific — VPC/CIDR,
  dual-region HA pairs, firewall-controller, account ACL, day-0 provisioning —
  which otomi has no model for; or (b) a place where LLZ already has a
  **more principled implementation** than vanilla otomi (overlay-reconcile vs
  `otomi commit`, static-seal auto-unseal, spec-owned values, token-inventory).
  Kept variations are re-homed *under* the `apl` surface, not deleted.

### Phasing

- **Phase 0 — Boundary & topology.** Introduce the `apl` noun-verb groups and draw
  the APL-layer (cloud-agnostic; talks to a running APL's values/API/git tree) vs
  provider-layer (Linode day-0, fleet) line in code. Nothing moves yet.
- **Phase 1 — Spec convergence.** APL portion → otomi schema 1:1; LLZ-only fields
  → `spec.landingZone.*`. Stop superset-owning where upstream has a schema.
- **Phase 2 — Values/overlay as the front door.** Promote `--reconcile-apl-overlay`
  to typed `apl set` / `apl team add` / `apl app enable` → ff-retry merge into
  `apl-<env>`. Interoperate with `otomi commit` / `[ci skip]`, do not fight it.
- **Phase 3 — Provider abstraction.** Extract Linode day-0 behind a
  `cluster-provider` interface; Linode is the default and richest provider.
  Largest keep-variation; demoted from "the foundation" to "a substrate."
- **Phase 4 — Collapse the managed/self-install fork.** "Managed" becomes a
  discovered attribute of a running APL, not a code fork; self-install helm path
  survives as one provider deployment mode.
- **Phase 5 — Secrets/identity consolidation.** `apl secret` / `apl identity` over
  OpenBao/PAT/harbor; defer to APL's ESO story where it fits, keep static-seal /
  token-inventory / ADR-0001 PAT locus where LLZ is stronger.
- **Phase 6 — Health/compliance as `apl doctor`.** Re-home CIS evidence,
  assert-image-fresh, sc-default-patcher, reconciler backstops under APL vocab.

## Consequences

- A coherent `apl …` operator surface emerges without a rename or a binary split;
  existing `llz` invocations keep working throughout.
- The `LandingZone` spec splits into an upstream-tracking APL portion and a
  namespaced `spec.landingZone.*` for Linode/fleet fields; some fields LLZ
  currently superset-owns are ceded back to otomi's schema.
- Linode day-0 becomes a provider plugin. This is the highest-churn phase and the
  one most likely to surface hidden Linode-specific coupling (VPC drops on raw
  API, g8 node guards, API-driven ACL).
- Tracking upstream schema means inheriting otomi values-schema changes; a
  version-skew guard (pin + diff on bump) is needed so upstream drift fails loud
  rather than silently mis-rendering.
- The kept variations (overlay reconcile, static-seal, spec-owned values,
  token-inventory, fleet/HA orchestration, CIS evidence) are explicitly LLZ's
  opinionated superset over vanilla otomi — retained, re-labeled, not competed away.

## Revisiting

Reopen if (a) upstream ships a first-class operator-facing `apl` CLI that covers
the values/teams/apps surface — convergence should then track *that* rather than a
parallel LLZ surface; or (b) LLZ gains a genuinely non-Linode provider, at which
point the Phase 3 `cluster-provider` interface graduates from a refactor seam to a
load-bearing product boundary and may justify the separate-binary topology
rejected here.

## Appendix A — Target `apl` command tree & package boundary (Phase 0)

The `apl` subtree is additive: it introduces APL-domain noun-verb groups in the
same binary, each **delegating to existing implementations** rather than
reimplementing them. Nothing is deleted in Phase 0; the provider/fleet commands
keep their current names and grouping.

### Command tree

```
llz apl                         # APL layer — cloud-agnostic, talks to a running APL
  team   add | list | login     # ← llz env(team bits) + team-login-smoke   (otomi teams)
  user   add                    # HOMED here; top-level `llz users` retired    (Keycloak users)
  app    enable | disable | list# WIRED — enable/disable edit env spec + re-render (otomi apps)
  values set | render | validate| show   # render + validate WIRED; set/show later
                                # ← llz render + reconcile-apl-overlay + validate-apl-values
  openbao get|set|exec|login    # WIRED — platform secret store (OpenBao KV). NO unified `apl secret`:
                                #   the two backends stay distinct; GitHub secrets remain `llz secrets`.
  status                        # ← llz status + verify (platform health)
  doctor                        # ← llz doctor + drift + verify           (APL-scoped health)

llz                             # provider + fleet layer — Linode day-0, unchanged
  new | env | network | import  # scaffold, deployments, VPC/CIDR, cluster import
  tokens | credentials | reap   # Linode credential + resource lifecycle
  up | build | upgrade          # provision/build/upgrade orchestration
  openbao | reconcile | ci      # secret internals, backstops, CI plumbing (below the waterline)
```

`llz apl values set x=y` is the flagship: typed edit → ff-retry merge into the
`apl-<env>` tree via the git-data API (today's `reconcile_apl_overlay.go`),
interoperating with apl-operator's `otomi commit` rather than fighting it.

### Package boundary

Two internal roots make the "two altitudes" split load-bearing in code, so the
`apl` cobra wiring is thin and the boundary is enforced by imports:

```
internal/apl/          # APL layer (cloud-agnostic)
  values/              #   otomi-schema-tracking types (spec.* APL portion)
  overlay/             #   git-data-API reconcile client (from reconcile_apl_overlay)
  team/ user/ app/     #   operations against a running APL (Keycloak, values tree)
  secret/              #   read/write surface (defers to APL ESO where it fits)

internal/provider/     # provider layer (Linode today)
  linode/              #   TF day-0 bootstrap, VPC/firewall, ACL, credentials, reap
  (ClusterProvider interface — Linode is the default & only impl at Phase 0)

cmd/llz/apl_*.go       # thin cobra wiring for the `apl` subtree → internal/apl
```

Rule of thumb: a file may import `internal/provider` **or** be imported by
`internal/apl`, never both — that's the boundary Phase 3 later hardens into the
`ClusterProvider` seam.

## Appendix B — Subcommand disposition (align / keep / demote)

Destinations: **Align** = re-homed under the `apl` surface, APL vocabulary,
tracks otomi schema. **Keep** = stays an LLZ variation (Linode/fleet-specific, or
a stronger-than-otomi implementation), re-homed under the provider/superset layer.
**Demote** = stays but below the waterline (CI/dev plumbing), not part of the
operator front door. **Split** = the command fans out — some verbs align, others
keep.

| Current (top-level) | Disposition | Destination / note |
|---|---|---|
| `users` → `apl user` | **Align (done)** | Extracted to `internal/apl/identity` (Phase 1) and **retired from the top level** — `apl user` is its sole home. First realized disposition. |
| `components` | **Align (done)** | `apl app` — `list` (registry) + `enable`/`disable <app> --env` (edit env spec + re-render, the GitOps source). `llz components` stays as the top-level list. |
| `render` | **Align (done)** | Wired as `apl values render` — the front door; provider-specific bits stay behind the provider layer. (Top-level `llz render` still exists, not yet retired.) |
| `validate` (`validate-apl-values`) | **Align (done)** | Wired as `apl values validate` — surfaced from `llz ci validate-apl-values` as a first-class values command |
| `status` | **Align** | `apl status` |
| `doctor` | **Align** | `apl doctor` |
| `env` | **Split** | env-as-APL-env → `apl` env selector; `vpc`/`peer`/`role` → keep (fleet) |
| `spec` | **Split** | APL portion → otomi schema (align); `spec.landingZone.*` → keep |
| `import` | **Split** | `import aplvalues` → align; `import cluster`/`linode` → keep |
| `secrets` | **Keep** | GitHub build-time secrets — provider/CI plumbing, stays `llz secrets`, **out of `apl`**. (Decision: no unified `apl secret` — the two secret backends stay distinct.) |
| `openbao` | **Align (done)** | Surfaced as `apl openbao` — the platform runtime secret store (OpenBao KV). static-seal / bao-seed internals → keep as CI plumbing. |
| `reconcile` | **Split** | `reconcile-apl-overlay` → `apl values set` (align); sc-demote / openbao / tokens backstops → keep |
| `verify` | **Split** | `verify-object-storage` etc. → `apl doctor` (align); provider verifies → keep |
| `drift` | **Split** | APL config drift → `apl doctor`; provider/TF drift → keep |
| `upgrade` | **Split** | `assert-apl-version` → align; cluster/instance upgrade → keep |
| `new` | **Keep** | scaffold LLZ instance repo — landing-zone concept |
| `network` | **Keep** | VPC/CIDR — Linode/fleet, no otomi model |
| `tokens` | **Keep** | credential provisioning — LLZ superset (provider) |
| `credentials` | **Keep** | Linode credentials — provider |
| `reap` | **Keep** | Linode resource reaping — provider |
| `up` / `build` | **Keep** | full provision→deploy orchestration — fleet/provider |
| `self-update` | **Keep** | CLI tooling |
| `ci` (large group) | **Demote** | CI/reconciler plumbing; a few (`token-inventory`, CIS evidence, `assert-*`) resurface under `apl doctor`/compliance |
| `lint` / `fmt` / `check` / `hooks` / `precommit` | **Demote** | dev tooling |

The disposition is deliberately weighted toward **Keep/Split**: the Linode/fleet
surface is a genuine superset otomi has no model for, and several LLZ paths
(overlay reconcile, static-seal, spec-owned values, token-inventory) are retained
*because* they are stronger than vanilla otomi — aligned in vocabulary, not
competed away.
