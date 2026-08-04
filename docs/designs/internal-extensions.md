# Design: the internal-extension catalog — every file in package `main`, assigned once

**Status:** **Proposed** — analysed, not built. Nothing in the tree depends on this catalog; it is
planning input for [ADR 0014](../adr/0014-core-surface-budget.md) (the core-surface budget) and
issue #10 / issue #399 (the extension framework), and not a commitment to a final extension list.
What it *is* is a measurement: what decomposition is available, and in what order.

**Measured:** 2026-08-03, against `feat/core-surface-budget` (214 non-test files, 41,709 logic
lines). Line counts are `llz ci core-surface --verbose`, so they are the same numbers the budget
gate enforces and can be re-derived at any time. Spot-checked on merge: `ci_health.go` 1,097,
`reconcile.go` 541, `ci_teardown.go` 492, and the eight `import-brownfield` files summing to
exactly 3,133.

**Re-measured 2026-08-05 on rebase onto `main`: 236 files, 47,182 lines.** The tables below are the
2026-08-03 measurement and are left at those numbers; [Re-measured on
rebase](#re-measured-on-rebase-2026-08-05) carries the delta, assigns every file that appeared in
between, and reconciles the two totals exactly. Read the tables for the *assignment* and that
section for the *current* numbers.

**This document owns the MEASUREMENTS** — the extension inventory, the line counts, the residue,
the grant distribution, the suggested ordering. [ADR 0014](../adr/0014-core-surface-budget.md)
owns the decision to budget package `main`; [the model](internal-extension-model.md) owns the
bindings-and-grants spec. Numbers belong here and are cited, not copied, from there.

Every one of the 214 non-test files in `tools/cmd/llz` assigned exactly once. Verified: sums to
**41,709**, no file unassigned, no file double-counted.

**57 internal extensions covering 37,945 lines (91%). Core residue: 3,764 (9%).**

Columns: `always` = would ship enabled on every instance (universality is a registry fact, not a
mechanism one); `ext?` = could plausibly become *external* later (pure argv action, no in-process Go).

---

## Re-measured on rebase (2026-08-05)

Two days of calendar, five weeks of `main`. The catalog measured 214 files / 41,709 lines; the gate
now reports **236 files / 47,182 lines**. Nothing was decomposed in between, so the entire move is
growth — which is the trend [ADR 0014](../adr/0014-core-surface-budget.md) predicted, arriving on
schedule and without anyone deciding it should.

The total reconciles exactly, in three parts:

| | lines | files |
|---|---:|---:|
| 2026-08-03 measurement | 41,709 | 214 |
| files that did not exist then | +4,604 | +22 |
| growth inside files already assigned | +869 | — |
| **2026-08-05 measurement** | **47,182** | **236** |

The 869 is *not* redistributed across the rows below — it is spread thinly over files the tables
already place, and attributing it would mean re-deriving every row for no change in conclusion. The
22 new files **are** assigned, because two of them are new extensions and one is the largest single
addition to any row:

| file | lines | assigned to |
|---|---:|---|
| `ci_docs_guard` 692, `ci_gen_toc` 90 | 782 | **`guard-docs` — new.** Pure file-in/findings-out, externalisable, `always`. The seventh gate. |
| `objproxy` 347, `objproxy_resign` 317, `objproxy_inject` 87 | 751 | **`obj-proxy` — new.** A long-running in-cluster process, not a verb: like `reconciler-runtime`, it should also become its own binary. |
| `ci_assert_obj_encryption` 500, `ci_obj_encryption_harbor` 254, `s3_ssec_probe` 151 | 905 | `assert-objstore` (560 → 1,465) |
| `template_commit` 213, `ci_upgrade_test_gate` 305 | 518 | `template-sustain` (630 → 1,148) |
| `build_preflight` 233, `status_preflight` 51 | 284 | `config-readiness` (733 → 1,017) |
| `ci_drain_obj_buckets` | 277 | `teardown` (1,070 → 1,347) |
| `ci_budget_gate` | 261 | `guard-budgets` (646 → 907) |
| `ci_loki_prove_writes` | 182 | `assert-observability` (1,596 → 1,778) |
| `region_resolve` 89, `instance_root` 82 | 171 | `env-topology` (740 → 911) |
| `ci_assert_adopter_pin` | 158 | `assert-platform` (602 → 760) |
| `state_passphrase` | 138 | `credential-state-passphrase` (199 → 337) |
| `ci_seed_ssec_key` 107, `objprefix` 24 | 131 | `credential-objkey` (474 → 605) |
| `reconcile_apl_overlay_wait` | 46 | `reconcile-actions` (648 → 694) |

**57 extensions become 59, covering 42,549 lines (90%).** The residue is unchanged at 3,764 plus its
share of the 869 — no new file lands in core.

Three things worth reading off this rather than the raw total:

- **Every new file was assignable, and none of it belonged in core.** The catalog's claim was that
  package `main` is ~90% extension material misfiled as commands. Five weeks of unrelated authorship,
  by people not thinking about extensions, landed at the same ratio. That is the strongest
  independent evidence the assignment is real and not a retrofit.
- **Two of the five weeks' work were whole new extensions**, both of which the `check|tool` ceiling
  handles badly: `guard-docs` is a clean gate, but `obj-proxy` is a *daemon* — no `kind:` in the old
  vocabulary describes it, which is the same omission that banned the `→ seeded` group.
- **The 869 of drift inside existing files is the quieter number.** New files are visible in review;
  a hundred lines added to `ci_health.go` is not. `exact: true` is what makes that visible at all.

---

## Core residue — 3,764, and shrinking

`main` 475, `ci` 868, `commands` 416, `checks` 497, `ci_template_manifest` 284, `selfupdate` 237,
`hooks` 121, `kubectl_probe` 113, `ci_shared` 109, `guard_walk` 87, `ext` 60, `ci_guards` 60,
`ci_assert_suite` 332, plus `color`/`exec`/`tfbin`/`forge_env`/`spec_helpers`/`guard_corpus` 105.

> **Corrected on merge.** This line originally read `…/spec_helpers/instance` **131**. `instance.go`
> (40) belongs to `scaffold-instance` above, and `guard_corpus.go` (14) belongs here; the subtotal is
> 105. The error was confined to this sentence — the stated residue (3,764), the extension total
> (37,945) and the grand total (41,709) were all correct, and the exactly-once assignment holds:
> 3,659 (the thirteen named) + 105 = 3,764, and 37,945 + 3,764 = 41,709 across 195 + 19 = 214 files.

Three of those are placeholders that shrink as the catalog lands:

- **`ci.go` 868** is almost entirely `AddCommand` registration. Once verbs are extensions it becomes
  a loader call — call it 60.
- **`ci_assert_suite.go` 332** becomes the core-owned *required-assertion set* for the `verified`
  state (`Gating: true/false` is already exactly that table). ~80 lines survive.
- **`checks.go` 497** is the Gate driver host; most of it leaves with `guard-*`.

Two are load-bearing and stay:

- **`ci_template_manifest.go` 284** *is* the `own-paths` grant implementation. ADR 0014's corollary
  says one ownership authority, so this is core by construction, not by inertia.
- **`ci_shared.go` + `guard_walk.go` + `kubectl_probe.go`** are the shared libraries every extension
  links. They belong in `tools/internal/*`, not in an extension.

Realistic settled core: **~2,900**.

---

## `→ scaffolded` — grants: `own-paths`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `scaffold-instance` | 928 | 5 | ✔ | ✘ | `scaffold` 217, `scaffold_spec` 209, `wizard` 384, `instance` 40, `custom_layout` 78. Needs in-process spec types — internal permanently. |
| `render-apl-values` | 951 | 6 | ✔ | ✘ | `render` 537, `yamledit` 140, `components_cmd` 152, `apl*` 122. The value-producer; the one extension the whole product is downstream of. |

## `→ configured` — grants: `read-repo`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `token-inventory` | 1,473 | 6 | ✔ | ✘ | `tokens` 437, `ci_token_inventory` 330, `token_validate` 211, `ci_rotation_plan` 216, `token_capability` 167, `ci_validate_tokens` 112. **Wants splitting** — it contributes predicates at three states (`configured`, `seeded`, `operating`). Best single candidate for fine-graining. |
| `env-topology` | 740 | 4 | ✔ | ✘ | `topology` 245, `env_set` 219, `branchpolicy` 165, `envlist` 111. |
| `config-readiness` | 733 | 3 | ✔ | ✘ | `readiness` 255, `state` 242, `ci_preflight` 236. **This is the `configured` predicate** — the cleanest existing example of predicate code that's mis-filed as a command. |

## `→ provisioned` — grants: `cloud-mutate`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `import-brownfield` | 3,133 | 8 | ✘ | ✔ | The single biggest movable block, and the only large one that is genuinely optional. Externalisable — its four non-trivial core calls become `llz render` / `llz new` argv. **Two bindings, not one:** import *writes an instance repo* (`transition:scaffolded[read-repo, cloud-read, own-paths]`) and *adopts cloud substrate* (`transition:provisioned[cloud-mutate]`). An earlier draft declared it as one transition to `provisioned` holding `own-paths`, which the validator rejects — own-paths is only meaningful where files are written. |
| `cluster-bootstrap` | 964 | 2 | ✔ | ✘ | `ci_bootstrap_cluster` 771 + manifests 193. ADR 0011's payload. |
| `cluster-access` | 952 | 4 | ✔ | ✘ | `runner_acl` 458, `runner_acl_configmap` 201, `fetchkubeconfig_state` 192, `fetchkubeconfig` 101. |
| `cloud-firewall` | 394 | 2 | ✔ | ✘ | `ci_discover_firewall` 215, `ci_firewall` 179. |
| `tofu-driver` | 235 | 3 | ✔ | ✘ | `ci_tfoutput` 98, `ci_tfplan` 81, `ci_tfdestroy` 56. Thin — the real driver is `internal/terraform`. |

## `→ seeded` — grants: `secret-custody` (+ `cloud-mutate` / `cluster-write`)

This is the group the current `check|tool` ceiling makes **structurally illegal**, and it's 6,874 lines.

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `openbao-lifecycle` | 2,185 | 13 | ✔ | ✘ | init/configure/CA/breakglass/seal-key/login. The largest `→ seeded` block; holds root material. |
| `keycloak-provisioner` | 803 | 3 | ✘ | ✘ | `ci_keycloak_configure` 328, `users` 284, `gateway_alias` 191. Textbook optional-capability-that-seeds. |
| `database-provisioner` | 770 | 4 | ✘ | ✘ | `ci_rotate_dbadmin` 282, `pg_probe` 229, `ci_seed_dbadmin` 140, `ci_db_report` 119. |
| `credential-pat` | 630 | 4 | ✔ | ✘ | `credentials_pat` 201, `ci_rotate_broad_pat` 196, `ci_incluster_pat` 178, `ci_seed_broad_pat` 55. Also binds an `operating` invariant (age). |
| `openbao-seed` | 611 | 4 | ✔ | ✘ | `ci_bao_seed` 279, `ci_seed_special` 236, `bao_seed_all` 71, `secret_apply` 25. |
| `harbor-provisioner` | 551 | 4 | ✘ | ✘ | `ci_harbor_provisioner` 265, `ci_harbor` 191, kick 94. |
| `credential-objkey` | 474 | 4 | ✘ | ✘ | `credentials_objkey` 196, `ci_temp_objkey` 106, `ci_mint_objkeys` 87, `objcluster_resolve` 85. |
| `credential-linode` | 462 | 4 | ✔ | ✘ | `ci_rotate_linode_creds` 271, `credentials_lkeadmin` 99, `credentials` 74, `linode_token` 18. |
| `credential-state-passphrase` | 199 | 1 | ✔ | ✘ | Tofu state encryption key rollover. |
| `forge-env-seed` | 189 | 3 | ✔ | ✘ | `gh_secrets_native` 63, `ci_github_oidc` 63, `ci_clear_secrets` 63. Should route through `internal/forge`. |

## `→ converged` — grants: `cluster-write`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `converge` | 1,599 | 5 | ✔ | ✘ | `ci_health` 1,097, `ci_wait` 216, `statushealth` 114, `wait_apl_pipeline` 96, `nudge_argo` 76. **The acid test.** The action/assertion split it needs is already built and can be copied rather than invented: `internal/health` is 1,164 lines of pure classification that `ci_health.go`'s header calls "the tested internal/health predicate", with the command reduced to the kubectl orchestration feeding it. That library half is an `assertion:converged`; the command half is the `transition:converged`. |
| `apl-upgrade` | 306 | 2 | ✔ | ✘ | `ci_managed_lock` 230, `ci_prepare_apl_upgrade` 76. |
| `argocd-diagnostics` | 243 | 1 | ✔ | ✔ | Read-only; pure argv. |
| `kyverno-policies` | 180 | 1 | ✔ | ✘ | Writes policy — `cluster-write`, so not a `check`. |

## `verified` — assertion contributors, grants: `cluster-read`

Each contributes assertions to the `verified` predicate; the core owns which are *required*. Every
one of these is externalisable — read-only, argv-shaped, already a lane in `assert-suite`'s table.

| extension | lines | files | always | notes |
|---|---:|---:|:-:|---|
| `assert-observability` | 1,596 | 10 | ✔ | scrape 228, `ci_readiness` 225, alert-eval 213, log-ingestion 190, alert-delivery 193, grafana 164, `prom_query` 140, prom-rules 104, prom-metrics 70, `loki_query` 69 |
| `assert-secrets` | 995 | 4 | ✔ | rotation-health 340, eso-roundtrip 266, broad-pat-rotation 204, openbao-audit 185 |
| `assert-network` | 840 | 4 | ✔ | network-enforcement 440, admission-enforcement 240, net-probe 83, wave-health-vap 77 |
| `assert-reconciler` | 725 | 2 | ✘ | 433 + effects 292 — pairs with `reconciler-runtime` |
| `assert-storage` | 631 | 3 | ✔ | volume-encryption 265, reconcile-volume-tags 203, relabel-volumes 163 (holds `cloud-mutate` — the odd one out) |
| `assert-identity` | 627 | 2 | ✔ | team-login-smoke 469, certificates 158 |
| `assert-platform` | 602 | 5 | ✔ | health-workflow 210, argo-app 130, instance-custom 106, image-fresh 82, apl-version 74 |
| `assert-objstore` | 560 | 3 | ✘ | obj-roundtrip 307, `s3_object` 131, `s3_probe` 122 |
| `assert-registry` | 381 | 1 | ✘ | harbor-roundtrip — pairs with `harbor-provisioner` |
| `wedge-gameday` | 224 | 1 | ✘ | negative/chaos testing; `cluster-write`, so not a plain assertion |
| `assert-database` | 194 | 1 | ✘ | pairs with `database-provisioner` |

**The pairing pattern is the strongest structural signal in the catalog.** `harbor-provisioner` ↔
`assert-registry`, `database-provisioner` ↔ `assert-database`, `reconciler-runtime` ↔
`assert-reconciler`, `keycloak-provisioner` ↔ `assert-identity`. A capability and its assertion turn
on and off together. That is an argument for **one extension carrying both bindings** rather than two
extensions — worth deciding early, because it halves the count and makes enablement coherent.

## `invariant: operating`

The binding the current design has no room for; without it these 4,283 lines stay core-special.

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `reconciler-runtime` | 1,094 | 5 | ✔ | ✘ | `reconcile` 541, leader 199, manager 146, health 125, convergence 83. The loop + leader election. **Should also become its own binary.** |
| `posture-credential-coverage` | 664 | 2 | ✔ | ✔ | `ci_extsecret_paths` 456, `ci_credential_coverage_guard` 208 |
| `reconcile-actions` | 648 | 7 | ✔ | ✘ | es-store-recovery 141, openbao 135, tokens 116, apl-overlay 106, argo-nudge 81, sc-demote 39, linode-token-wait 30. **Seven separate invariants** — the clearest case for one-invariant-per-extension. |
| `posture-plaintext` | 626 | 1 | ✔ | ✔ | The largest single guard and the most instance-tunable (its protocol allow-list is policy, not fact). Best stress test of the vehicle. |
| `health-sla` | 405 | 3 | ✔ | ✔ | sla 165, readiness 162, incluster 78 |
| `posture-mesh` | 364 | 2 | ✘ | ✔ | mtls-wiring 211, mesh-egress 153 |
| `posture-at-rest` | 304 | 1 | ✔ | ✔ | |
| `wave-health` | 178 | 1 | ✔ | ✔ | |

## `→ promoted` / `→ upgraded` / `→ destroyed`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `release-publish` | 1,150 | 5 | ✘ | ✘ | chart-publish-check 366, `gh_gitdata_native` 239, pin-images 204, publish-charts 187, deliver-docs 154. Template-repo-side, not instance-side. |
| `teardown` | 1,070 | 4 | ✔ | ✘ | `ci_teardown` 492, `reap` 328, destroy-unwedge 207, crd-unwedge 43 |
| `template-sustain` | 630 | 5 | ✔ | ✘ | `upgrade_policy` 236, `drift` 114, `template_removals` 94, `upgrade_churn_guard` 107, `stamp` 79. Consumes the `own-paths` grant. |
| `promote-pipeline` | 307 | 2 | ✔ | ✘ | `promote_gen` 173, `promotion` 134. Already a codegen DAG — same shape as `extension_ci.go`; **the two should share one emitter**. Grants `read-repo` only: its output `promote.yml` is a copier-rendered `merge` stub, so it does *not* want `own-paths` (see Decision 1). |

## Gate — attach at `→ scaffolded` / `→ configured`, grants: `read-repo`

Pure file-in/findings-out. All six externalisable; none needs a cluster or a credential.

| extension | lines | files | always | notes |
|---|---:|---:|:-:|---|
| `guard-budgets` | 646 | 3 | ✔ | untestable-loc 447, coverage 166, core-surface 33. **Start here** — the gate exports itself. |
| `guard-charts` | 546 | 4 | ✔ | chart-lock 148, chart-pin 143, chart-version 130, cosign-subject 125 |
| `guard-monitoring` | 452 | 3 | ✔ | wave-dependency 222, prom-rules 154, monitoring-label 76 |
| `guard-manifests` | 351 | 4 | ✔ | argocd-rendered-apps 123, apl-schema 111, placeholder 77, dropped-apiversions 40 |
| `guard-pins` | 279 | 2 | ✔ | version-pins 254, pin-coherence 25 |
| `guard-workflows` | 101 | 1 | ✔ | check-workflow-shells |

## Dev-only — template-repo tooling, never ships to an instance

| extension | lines | files | ext? | notes |
|---|---:|---:|:-:|---|
| `phase-timing` | 316 | 2 | ✔ | phase-mark/report 201, image-pulls 115 |
| `dev-mutation-testing` | 265 | 1 | ✔ | `ci_mutate` — gremlins wrapper |
| `verify-lab` | 239 | 2 | ✔ | `verify` 170, `sshcheck` 69 |
| `doctor-probes` | 230 | 3 | ✔ | doctor-linode 93, doctor-crossorg 104, credentials-probe 33 |

---

## What the catalog says

**57 is too many, and that's the useful finding** — 59 after the rebase re-measurement, and the
direction of that revision is itself the point. The pairing pattern (`*-provisioner` ↔ `assert-*`)
collapses ~8 pairs into 8 extensions with two bindings each, taking the count to **~49**. Splitting
`token-inventory` (3 states) and `reconcile-actions` (7 invariants) pushes it back up. Somewhere
around **45–55 internal extensions** is the real shape — which means the loader, the registry, and
`llz extension list` need to be built for dozens, not for the handful the spike assumes.

**Universality is not the discriminator — 41 of 57 are `always`, against 16 opt-in.** That is the
whole point of the reframe: if the framework carries only the opt-in ones it relieves ~9,400 lines;
carrying all 57 relieves 37,945.

`always` is a **default, not a constant**, and the assert lanes are what settle it. `llz ci
assert-suite` is invoked from three places in `instance-template/` — unconditionally in
`llz-bootstrap-openbao.yml`, as a six-lane subset in `llz-cluster-health.yml`, and on a schedule in
`llz-scheduled-checks.yml`. So the battery is instance-facing, not template-repo-only, and an
instance with no object storage has to disable `assert-objstore` in its own configuration rather than
by taking a different build. The registry must let an instance override this field in both
directions.

**29 of 57 are externalisable** — the 12 flagged in the tables, plus the 11 assertion contributors and
6 gates, all of which are read-only and argv-shaped. The other 28 need in-process Go: spec types,
credential handles, cluster clients. **That ratio is the argument for the internal Go action ABI
being built first**, not the remote-fetch half the spike front-loaded.

**Grant distribution:** `cluster-read` 18 · `read-repo` 14 · `secret-custody` 11 · `cloud-mutate` 10 ·
`cluster-write` 10 · `own-paths` 5 · `cloud-read` 2. No grant is held by a majority, and
`secret-custody` — the one the `check|tool` ceiling banned outright — is concentrated in one
transition plus two invariants.

Read that distribution as a **design intuition, not a measurement**: the grants below were assigned
in the same pass that invented the vocabulary, so the spread reports this document's judgement rather
than an independent property of package `main`. It becomes evidence when extensions declare their own
grants and the distribution is observed instead of assigned.

## Suggested first five

1. **`guard-budgets`** (907) — self-hosting proof, zero grants beyond `read-repo`, already unit-tested.
2. **`converge`** (1,599) — the acid test, run early rather than deferred. Forces the Go action ABI
   and the action/predicate split in `ci_health.go` on day one, which is where the design either
   holds or doesn't.
3. **`import-brownfield`** (3,133) — biggest single relief, genuinely optional, and the first real
   exercise of `alwaysEnabled: false`.
4. **`assert-observability`** (1,778) — first assertion contributor; proves the `verified` required-set
   table replaces `assert-suite`'s scheduler rather than sitting beside it.
5. **`reconciler-runtime`** (1,094) — first invariant binding, and the largest inward extraction the
   extension conversation has been crowding out.

Cumulative: **8,511 lines out of package `main`** across five extensions, exercising every binding
type (gate, transition, assertion, invariant), both enablement modes, and four of the seven grants.

## Decisions

Four questions the declaration model raised. All four are settled; two changed the code and two
constrain the driver slice that has not been written yet.

### 1. Generated files: `own-paths` is a fence against copier, not a claim on authorship

The per-file question turned out to be already answered by `.template-manifest`, whose classes turn
on **who renders the bytes at upgrade time** — not on who wrote them first:

| the bytes come from | class | example |
|---|---|---|
| copier, token-free, template owns them outright | `managed` | the vendored `llz-*.yml` reusable bodies, `apl-values/_shared/apl-overlay/**` |
| copier, carrying fork-local tokens or an operator-tunable trigger surface | `merge` | the workflow caller stubs — `terraform.yml`, **`promote.yml`** |
| anything that is not copier | `owned` | `apl-values/*/**` (from `llz render`), `.terraform.lock.hcl` (from `terraform init`), `kubernetes-custom/**` (from the operator) |

`own-paths` **is** the `owned` class. So the grant does not mean "this binding writes files"; it means
"copier must not render these bytes, because something else does." Authorship is irrelevant — llz
generates `apl-values/*/**` and it is `owned`; llz also generates `promote.yml` and it is `merge`,
because copier renders that one too.

**Which is why restricting the grant to `scaffolded` and `upgraded` is principled rather than
conventional:** a fence only matters when the thing it fences off runs, and copier runs at exactly
two moments — `llz new` and `copier update`. A binding that writes a file at some *other* state does
not need the grant; it needs its extension to have declared the fence once, at one of the two moments
copier could otherwise have clobbered it.

Consequences: `promote-pipeline` does **not** want `own-paths` (its output is `merge`), and the
validator was right to refuse it. The catalog row was wrong and is corrected. A future
`llz-extensions.yml` generated from the per-instance *enabled set* cannot be copier-rendered at all,
so it is `owned` and its extension declares the fence at `scaffolded`/`upgraded` — which the existing
rule already permits.

### 2. `llz state` recomputes, with a freshness window

Cheap predicates are evaluated every time; expensive ones (the gating assert lanes, minutes of
cluster round-trips) reuse a recorded result inside a TTL and re-run past it. A recorded-only state
would let `llz state` report a station the instance has since drifted out of, and always-recompute
would make the command too slow to reach for — which in practice means nobody runs it, and an
unobserved state machine is a diagram.

This binds the driver slice: every state needs a predicate with a declared cost and a TTL, and
`llz state` must be able to say *when* it last actually looked.

### 3. Grants are enforced — the grant IS the handle

A `cluster-read` binding receives a read-only kubeconfig; a `secret-custody` binding receives an
OpenBao token fenced to its declared paths. Grants are capability scoping, not review metadata.

Two things follow. A binding declaring **no** grants is handed nothing and cannot run, so it is now
rejected as an incomplete declaration (it previously validated). And the action ABI has to be
designed around *delivering* capabilities rather than passing one context to everyone — which makes
the ABI's shape a security question, not only an ergonomics one.

### 4. A required assertion may be waived, with a reason and an expiry, in the spec

The core keeps ownership of the required set; the waiver is a spec-level, attributable, expiring
exception — the same shape this repo already uses for coverage and credential exemptions. What it
must not become is a per-instance redefinition of `verified`: the waiver records that a known
assertion is being skipped and until when, so a stale one is visible rather than absorbed.

This also answers what advances `verified` and `operating` — the driver evaluates the required set
minus live waivers. An extension never declares its own state reached.
