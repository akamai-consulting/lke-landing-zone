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
| `ci_docs_guard` 692, `ci_gen_toc` 90 | 782 | **`guard-docs` — new.** `always`, the seventh gate. **✅ Extracted.** Marked `ext?` ✔ here and that was WRONG — see [The first ten, extracted](#the-first-ten-extracted). |
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
| `import-brownfield` | 3,133 | 8 | ✘ | ✔ | The single biggest movable block, and the only large one that is genuinely optional. Externalisable — its four non-trivial core calls become `llz render` / `llz new` argv. **Two bindings, not one:** import *writes an instance repo* (`transition:scaffolded[read-repo, cloud-read, own-paths]`) and *adopts cloud substrate* (`transition:provisioned[cloud-mutate]`). An earlier draft declared it as one transition to `provisioned` holding `own-paths`, which the validator rejects — own-paths is only meaningful where files are written.  **✅ Extracted** — the first opt-in, and `ext? ✔` was WRONG; see [The first ten, extracted](#the-first-ten-extracted).|
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
| `assert-storage` | 631 | 3 | ✔ | volume-encryption 265, reconcile-volume-tags 203, relabel-volumes 163 (holds `cloud-mutate` — the odd one out). **✅ Extracted** — the flag was a defect report, not a footnote; see [The first ten, extracted](#the-first-ten-extracted). |
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
| `reconcile-actions` | 648 | 7 | ✔ | ✘ | es-store-recovery 141, openbao 135, tokens 116, apl-overlay 106, argo-nudge 81, sc-demote 39, linode-token-wait 30. **Seven separate invariants** — the clearest case for one-invariant-per-extension. **◐ Four of eight extracted** — and `linode-token-wait` is not a lane at all; see [The first ten, extracted](#the-first-ten-extracted). |
| `posture-plaintext` | 626 | 1 | ✔ | ✔ | The largest single guard and the most instance-tunable (its protocol allow-list is policy, not fact). Best stress test of the vehicle. |
| `health-sla` | 405 | 3 | ✔ | ✔ | sla 165, readiness 162, incluster 78 |
| `posture-mesh` | 364 | 2 | ✘ | ✔ | mtls-wiring 211, mesh-egress 153 |
| `posture-at-rest` | 304 | 1 | ✔ | ✔ | **✅ Extracted** — the first non-gate binding; see [The first ten, extracted](#the-first-ten-extracted). |
| `wave-health` | 178 | 1 | ✔ | ✔ | |

## `→ promoted` / `→ upgraded` / `→ destroyed`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `release-publish` | 1,150 | 5 | ✘ | ✘ | chart-publish-check 366, `gh_gitdata_native` 239, pin-images 204, publish-charts 187, deliver-docs 154. Template-repo-side, not instance-side. |
| `teardown` | 1,070 | 4 | ✔ | ✘ | `ci_teardown` 492, `reap` 328, destroy-unwedge 207, crd-unwedge 43 **✅ Extracted** — the first transition; `reap` and `drain-obj-buckets` stayed. See [The first ten, extracted](#the-first-ten-extracted). |
| `template-sustain` | 630 | 5 | ✔ | ✘ | `upgrade_policy` 236, `drift` 114, `template_removals` 94, `upgrade_churn_guard` 107, `stamp` 79. Consumes the `own-paths` grant. **◐ Partial** — the own-paths half cannot leave core (ADR 0014). See [The first ten, extracted](#the-first-ten-extracted). |
| `promote-pipeline` | 307 | 2 | ✔ | ✘ | `promote_gen` 173, `promotion` 134. Already a codegen DAG — same shape as `extension_ci.go`; **the two should share one emitter**. Grants `read-repo` only: its output `promote.yml` is a copier-rendered `merge` stub, so it does *not* want `own-paths` (see Decision 1). |

## Gate — attach at `→ scaffolded` / `→ configured`, grants: `read-repo`

Pure file-in/findings-out. All six externalisable; none needs a cluster or a credential.

| extension | lines | files | always | notes |
|---|---:|---:|:-:|---|
| `guard-budgets` | 646 | 3 | ✔ | untestable-loc 447, coverage 166, core-surface 33. **Start here** — the gate exports itself. **✅ Extracted** — see [The first ten, extracted](#the-first-ten-extracted). |
| `guard-charts` | 546 | 4 | ✔ | chart-lock 148, chart-pin 143, chart-version 130, cosign-subject 125 **✅ Extracted** (cosign-subject stayed — it is release-publish territory); see [The first ten, extracted](#the-first-ten-extracted). |
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

---

## The first ten, extracted

`guard-budgets` and `guard-docs` are no longer rows in a table.

```
$ llz extension list --verbose
NAME           ENABLED  BINDINGS                    GRANTS     SUMMARY
guard-budgets  always   gate:scaffolded             read-repo  cap untestable logic …
                        gate:scaffolded[read-repo]
guard-docs     always   gate:scaffolded             read-repo  fail when the docs name …
                        gate:scaffolded[read-repo]
```

| | budget | files | |
|---|---:|---:|---|
| before | 47,182 | 236 | |
| `guard-budgets` extracted | 46,716 | 234 | −466, the first paydown this gate ever recorded |
| `llz extension list` | 46,797 | 235 | +81, spent deliberately |
| `guard-docs` extracted | 46,106 | 235 | −691, the largest single move so far |
| `posture-at-rest` extracted | 45,763 | 234 | −343, and the first binding that is not a gate |
| `assert-storage` extracted | 45,229 | 232 | −534, and the first that touches a cluster and a cloud |
| `reconcile-actions` extracted | 44,826 | 228 | −403, four of eight lanes — the other four are the finding |
| `teardown` extracted | 44,171 | 226 | −655, and the first binding that MOVES the platform |
| `template-sustain` extracted | 43,817 | 221 | −354, partial by construction — and the model grew a word for that |
| `import-brownfield` extracted | 40,827 | 214 | **−2,990**, the largest single move — and the first opt-in |
| `obj-encryption` extracted | 38,821 | 206 | −2,006, and the first binding at `seeded` |
| `guard-charts` extracted | **38,364** | 202 | −457, and `guardwalk` — the traversal ten guards share |

**Net −8,818 (18.7%) across ten extensions**, and now *below* the 41,803 this gate first recorded —
the number the whole exercise started from. Read that as a floor on the effort rather than a
schedule, and read [the closure census](#the-cost-of-the-interesting-half) before reading this table
as a rate.

### What `guard-budgets` established

- **The ratchet moves down, and `exact: true` is what forces it into the diff.** Extract the code,
  forget the line, and the gate fails with `SHRANK — LOWER IT` and the number to write.
- **The declaration lives with the code, not in the registry.** A central table transcribing each
  extension's bindings would be a hand-maintained list beside the thing it describes. `registry.go`
  names packages; each package states its own bindings and grants.
- **Extraction surfaces shared rules the catalog cannot see**, because it assigns whole files and
  these were functions inside one: `internal/pathglob` (the glob dialect `llz ci template-manifest`
  also matches copier's fence entries with) and `internal/shquote` (the quoting rule `llz ci
  at-rest-guard` also strips Terraform comments with). Two callers had each been keeping a private
  copy, and their agreement was accidental.

### What `guard-docs` added, by disagreeing

`guard-budgets` fit the model perfectly, which proves less than it looks: a model tested only against
the case it was derived from is a tautology. `guard-docs` was picked because it had never been scored
by this catalog — it did not exist when the measurement was taken — and because part of it does not
fit. Three findings:

- **`ext?` was wrong, and this is the catalog's central claim demonstrated.** The delta table above
  marks `guard-docs` externalisable. It is not, and cannot be: the command/flag check resolves each
  documented `llz …` invocation against the **live cobra tree** and asks it whether a flag exists and
  whether it takes a value — including *hidden* flags, which is how a deprecated-but-working `--env`
  was once mis-reported as removed. An argv tool would have to re-derive the tree from help text,
  which is the second-implementation-of-a-shared-rule bug this code already has scars from. "36 of 57
  need in-process Go" now has one worked example instead of an estimate.
- **The model has no `write-repo`, and that is a real gap.** `llz ci gen-toc` writes Markdown back to
  disk, and nothing can express it. A `gate` may hold `read-repo` only — correctly. `own-paths` is
  the nearest-looking grant and is the wrong one: per [Decision 1](#1-generated-files-own-paths-is-a-fence-against-copier-not-a-claim-on-authorship)
  it means "copier must not render these bytes", a fence rather than a write permit, and the template
  repo's own `docs/` is not copier-rendered at all. **This is the second case, not the first:**
  `promote-pipeline` generates `promote.yml` and is recorded here as holding `read-repo` only, which
  is wrong on its face for the same reason. Two independent cases is enough to say the vocabulary has
  a hole and not enough to know its shape, so nothing was invented — the file split follows the
  declaration instead (rendering in the package, the `os.WriteFile` in the command), so what IS
  declared is true, and a test asserts the package contains no write path.
- **Extraction moves tests, and the per-package coverage ratchet reads that as a regression.** Six of
  27 test functions could not move, because they assert against the real command inventory; a
  synthetic tree would let them pass while the CLI drifted. `internal/docsguard` therefore measures
  **74%** package-local and **93%** with `-coverpkg` across both test packages, without a test being
  deleted. Every future extraction will hit this. Read a low floor on a freshly extracted package as
  "its tests are elsewhere" before reading it as "it is untested", and say which in the comment.

### What `posture-at-rest` added — the first binding that is not a gate

Two gates in a row left three of four binding kinds and nine of ten states unexercised. This one was
picked to move that, and it produced the first thing the model has said that reading the code does
not.

**The discriminator between a gate and an invariant is the claim's LIFETIME, not the
implementation.** `llz ci at-rest-guard` is a static Terraform scan — file-in, findings-out, exactly
like the two gates — and it is still an invariant, because encryption is decided at CREATE and is
immutable afterwards. There is no remediation for a resource that came up unencrypted, only a
rebuild, so the property has to hold across every future apply rather than at the moment someone ran
the checker. A gate claims something about a *change*; an invariant claims something about the
*running system*. The catalog classed this one correctly but never said why, and the reason was
invisible while everything declared was a gate.

**Declaring it honestly cost something, and the cost is recorded rather than smoothed over.** An
invariant may attach only to `operating`, and this scan runs in template-repo CI long before anything
is operating — so the declaration is a claim the driver cannot yet honour. Declaring `gate` would
have been the convenient lie, and it would have made the model agree with itself by making it say
nothing.

**A third extraction, a third shared rule.** `guardkit` — `RepoPath` (where a tree is, in the
template layout or the instance layout) and `RequireCorpus` (did the walk see anything at all) — was
private to two guard files and called by eight. Extracting one guard turned it into a package. This
has now happened on every extraction without exception.

### What `assert-storage` broke — the ceiling was wrong

The catalog flags exactly two entries as breaking its own assertion rule, and this is one of them:
*"holds `cloud-mutate` — the odd one out."* The model's answer was that such an entry is not an
exception but a **mis-declaration**: re-model it, and declare the mutating half as its own binding.
This is that claim, tested. It half survives.

**The re-modelling works.** Three bindings, and the first extension in the repo with more than one:

```
$ llz extension list --verbose
NAME            ENABLED  BINDINGS                              GRANTS                                SUMMARY
assert-storage  always   assertion:verified invariant:operating cloud-read,cluster-read,cloud-mutate  every PV-backed Linode Volume …
                         assertion:verified[cluster-read, cloud-read]
                         invariant:operating/volume-tags[cluster-read, cloud-mutate]
                         invariant:operating/volume-labels[cluster-read, cloud-mutate]
```

Folding the reconciler lanes into the assertion would need an assertion holding `cloud-mutate`, which
the validator refuses — correctly, since an assertion that mutates what it measures cannot be trusted
about what it found. Declared separately, the assertion keeps its read-only ceiling and the mutation
is still declared. This is also the first declaration to need `Binding.Name` (two invariants, and
`operating` is the only state one may attach to) and the first where the terse `GRANTS` union says
something no single binding does — which is exactly the misreading `--verbose` exists to prevent.

**And then the ceiling refused it.** `grantStates` listed `cloud-mutate` at `{provisioned, seeded,
converged, destroyed}` — **not `operating`** — so both invariants were rejected:

```
REFUSED: assert-storage: invariant:operating/volume-tags[cluster-read, cloud-mutate]:
  "cloud-mutate" may only be asked for at provisioned, seeded, converged, destroyed
```

Those two lanes are wired into `reconcile.go`. They run in-pod, continuously, and they PUT tags and
labels onto Linode Volumes. **The model was refusing to describe code that ships.**

Three things about that are worth keeping:

- **Refusing it was not conservative, it was wrong in the dangerous direction.** A continuously
  running cloud mutator is precisely what a reviewer most wants declared. A ceiling that makes it
  inexpressible does not prevent it — it only stops it being written down. That is `→ seeded`
  banned-by-omission again, occurring in the half of the ceiling that was added to fix
  banning-by-omission.
- **The catalog had the evidence and did not follow it.** It wrote "the odd one out" beside this very
  row and never carried the observation into the grant table, which inherited the blind spot. A
  flagged anomaly is a defect report, not a footnote.
- **The table said this would happen.** Its own header calls it *"judgement transcribed, not a derived
  fact … the most likely thing here to need a row added, and adding one should be an argued change
  rather than a quiet widening."* It was right, and the argument is recorded next to the row, with a
  test pinning the whole table so the next widening has to be argued too.

**The extraction also cost something the first three did not: seams.** `assert-storage` is the first
extension that touches a cluster and a cloud, and it needed four capabilities injected — a Linode
token, an in-cluster client, a kubectl shell-out, and a step-summary sink (`volumes.Deps`). Every
high-coupling candidate in the census below needs some subset of the same four. **That is the action
ABI's requirements document, arrived at by extracting rather than by design.**

A side effect worth naming: `AssertEncryption` and `ReconcileTags` were **0% covered** before, and
not for want of trying — as package-`main` functions they reached for the token, the client and
kubectl themselves, so a test could only have run them against a real cluster. Taking `Deps` as a
parameter is what made them reachable, and the package went 64.8% → 85.3%. Delivering capabilities
rather than letting code acquire them is a testability property before it is a security one.

### What `reconcile-actions` proved, and the gap it opened

The catalog calls this *"seven separate invariants whose needs differ — the clearest case for
one-invariant-per-extension"*, and it is the first claim the model gets unambiguously **right**.

```
reconcile-actions  invariant:operating/sc-demote[cluster-read, cluster-write]
                   invariant:operating/argo-nudge[cluster-read, cluster-write]
                   invariant:operating/es-store-recovery[cluster-read, cluster-write]
                   invariant:operating/openbao-gauges[cluster-read, secret-custody]
```

**The over-granting argument, as one number.** Collapse those into a single binding and it must hold
the union — `cluster-write` **and** `secret-custody`. The read-only OpenBao sampler would gain
permission to patch StorageClasses and Argo Applications; the three cluster lanes would gain an
OpenBao token. Neither needs the other's capability, and the sampler is the one that most obviously
must not have it. A test asserts no binding holds both.

`openbao-gauges` is also the registry's first **`secret-custody`** binding, and it needed no ceiling
change — `grantStates` already permitted it at `operating`. That makes it the control case for
`assert-storage`'s `cloud-mutate` row: the table is not simply permissive everywhere, and the earlier
correction was a genuine defect rather than the ceiling being too tight in general.

**Four of the eight lanes did not move, in three different ways:**

| lane | why it stayed |
|---|---|
| `tokens` | 15 references into `ci_token_inventory.go` — a *separate* catalog entry of 1,473 lines. The lane is 236 lines and its dependency is six times that. |
| `apl-overlay`, `apl-overlay-wait` | Share this package's OpenBao client seam. One credential cluster with `openbao-gauges`; splitting it to move half would mean duplicating the seam. |
| `linode-token-wait` | **It is not a lane.** It is a watch *wrapper* that kicks another lane when the Linode token first appears, and never acts itself. The catalog counts it among the seven invariants; it belongs to the runtime. A miscount found only by trying to declare it. |

**And the gap: the model cannot say an extension is PARTIAL.** `reconcile-actions` declares four
bindings and reads as complete. Nothing distinguishes *"has four invariants"* from *"has eight, four
of which are still in core"*. Every extraction from here passes through this state, and an extension
that silently under-declares its own surface is the same failure shape as banning by omission — the
reader cannot tell what is missing.

Recorded, not fixed. The remedy is probably a declaration-level `incomplete` marker naming what is
outstanding, and inventing one from a single case is exactly what the [`write-repo`
deferral](#what-guard-docs-added-by-disagreeing) was right to avoid. It becomes actionable at the
second partial extension.

**A fourth shared rule, and a boundary tax.** `transientFetchError` — the transient-vs-permanent
classification for Argo comparison errors — was private to `ci_assert_argo_app.go` and called by the
nudge lane; it moved to `internal/health`, beside `IsGitAuthError` which it already called. Two
callers disagreeing about whether a timeout is permanent is a lane that never settles. Separately,
two *coupling* tests now span the new boundary and needed symbols exported purely so they could still
reach both halves (`ObjReadyStatus` vs the runtime's `readyCondition`; `CredPaths` vs
`policyReconcilerRead`). That is a real cost of extraction and worth budgeting for: a test that
cannot reach both things it couples is not a test.

### What `teardown` proved — the first `transition`

Six extensions in, the first binding that **moves** the platform. The previous five all observe
(`gate`, `assertion`) or hold (`invariant`).

```
teardown  transition:destroyed [cloud-mutate, cluster-write]   the destroy itself
          assertion:destroyed  [cloud-read]                    assert-no-orphans
```

**It validated with no ceiling change.** `cloud-mutate` was already legal at `destroyed` — the one
state in `grantStates` that exists for exactly this. It also declares **the model's own motivating
example**: `validate.go` justifies letting assertions target any state by naming `assert-no-orphans`,
*"the assertion that `destroyed` actually holds, and the one a missed Volume bills for."* That
argument had never been tested against code. It holds.

**The read-only ceiling earns itself here.** `assert-no-orphans` is the destroy job's *final* gate.
An assertion able to delete could clean up whatever it found and then report zero — and this is where
that shortcut is most tempting, since the reaper is one call away.

**A third thing the model cannot say: `Confirm`.** Every seam the previous five needed delivers a
*capability*. `teardown.Deps.Confirm` delivers an **authorisation** — `--yes`, the answer to "may I",
not the means to act. `cloud-mutate` says this binding MAY delete cloud resources; nothing says a
human agreed to **this** deletion. Unlike the other two gaps this is probably not a missing grant but
a missing axis, and it belongs to the action ABI.

**A fifth shared rule, and the sharpest.** The orphan census — what counts as a leaked Volume,
NodeBalancer or VPC — had **three** consumers: preflight reports it, `assert-no-orphans` gates on it,
`llz reap` cleans by it. Its comment said so outright and nothing but proximity kept them in step. An
under-counting census makes preflight optimistic but makes the destroy gate **pass on a leak** — a bug
already paid for once, when it counted only `pvc-`-prefixed Volumes and every relabelled Volume was
invisible to it.

**And a cost of the `Deps` pattern**, found by the first extraction big enough to feel it: `Deps{}`
compiles and then dereferences a nil func the moment a path needs a seam the caller forgot. Still the
right trade, but a future action ABI should hand extensions **zero values that work** — `Confirm`
defaulting to nil would mean a destroy verb panicking instead of refusing.

### What `template-sustain` proved — the prediction held, and the model adapted

The one the closure census said might not be separable, picked *because* of that. The prediction held
exactly: **what asks about provenance moved; what acts on the manifest did not.**

```
template-sustain  assertion:upgraded [read-repo]   drift — how far behind is this instance
                  gate:scaffolded    [read-repo]   the upgrade-churn guard
                  NOT DECLARED: transition:upgraded[own-paths] …
```

**`own-paths` may be the one grant no extension can ever hold.** It belongs to the copier
restore/overwrite pass, which reads `.template-manifest`'s class table — and ADR 0014's corollary says
there is exactly **one** ownership authority. The extension that would hold `own-paths` is welded to
the file that *defines* it. That is a question the corollary opens and does not answer.

**The model grew `Incomplete`.** `reconcile-actions` arrived partial first; this is the second, with a
different cause — a sibling's territory versus a core-by-construction file. Two independent
occurrences is the bar every vocabulary change here has used, so `Extension.Incomplete` now exists,
both declarations say what they are missing, and `llz extension list` marks them `always ◐` in the
column a skimmer actually reads. It is prose rather than a schema: `Validate()` only refuses a blank
entry, because a marker that says "partial" without saying how is worse than no marker.

**A sixth shared thing, and the most mundane: colour.** `internal/color` — seven one-line wrappers and
the `NO_COLOR`/TTY rule. It became a package on the *seventh* extraction rather than the first
because colour is what a verb does at the very end, so everything that prints needs it, and six
copies of `paint()` would be six chances to disagree about when to stay quiet.

### What `import-brownfield` settled — and one correction it forced

The catalog's own worked example, transcribed by hand in `TestCatalogSampleIsExpressible` where it
explains multi-binding transitions. Declaring it against real code was therefore also a test of that
transcription. **It was correct** — bindings and grants both.

```
import-brownfield  opt-in ◐  transition:scaffolded  [read-repo, cloud-read, own-paths]
                             transition:provisioned [cloud-mutate]
```

**The first `alwaysEnabled: false`.** Seven extensions shipped `always`, leaving the field with one
observed value — a default nobody had ever set the other way. Adoption settles it: a greenfield
instance has nothing to import, so this capability is needed once or never.

**`own-paths` is reachable after all, and that corrects the previous extraction.** `template-sustain`
suggested it might be the one grant no extension can ever hold, because the copier restore pass reads
`.template-manifest`'s class table and ADR 0014 pins that as core. Import shows the distinction:
**writing** a file the manifest classes `owned` needs no access to the class table. The grant is a
*fence* — "copier must not render these bytes" — and declaring a fence is not enforcing one. What is
unreachable is the restore pass, not the grant.

**The catalog's most specific prediction held.** It said import's *"four non-trivial core calls become
`llz render` / `llz new` argv"*, and the closure found exactly those — `llz new`, `llz env add`, the
two spec editors, `llz render` — plus a kubectl seam and two constants.

**What it got wrong is the conclusion:** `ext? ✔`. Those calls are not argv-shaped. `EnvAdd` takes a
struct import fills from a live cluster; `EditSpec` takes a `func(*yaml.Node) error` so the spec keeps
its comments. An external tool would have to re-implement comment-preserving YAML editing and the
env-add option surface. That is the **second** time `ext?` has been wrong in the same direction, after
`guard-docs` — the column is optimistic by construction, answering from a verb's outputs rather than
its inputs.

### What `obj-encryption` settled — the group the old ceiling banned

PR #15's `kind: check|tool` menu had no seeder skeleton, so the whole `→ seeded` group — 6,874 lines
of credential provisioning — was inexpressible **by omission**. That omission is the single defect the
declaration model was built to fix. This is the first extension to occupy the state, which makes it
the test of whether the fix was real or only argued.

**It was real. No ceiling change was needed.**

```
obj-encryption  opt-in ◐  transition:seeded    [secret-custody]
                          invariant:operating  [secret-custody, cluster-read]
                          assertion:verified   [cluster-read, cloud-read]
```

**Custody here is literal.** Linode Object Storage implements exactly one server-side encryption mode
— SSE-C, where the *client* supplies the key on every request (SSE-S3 answers 400, PutBucketEncryption
answers 501). There is no arrangement where the provider holds the key: the platform mints 32 bytes,
keeps them in OpenBao, and hands them to the one process that injects them into every write.

**Three moments, three grant sets, and the risk differs at each:**

- The **seed** runs once and touches nothing but OpenBao — no cluster grant, because it does not read
  the cluster. The refusal that protects everything lives here: an *indeterminate* read must never be
  treated as "no key present", because minting a second key makes every object written under the
  first unreadable. `Deps.KVGet` returns three values, not a bool, for exactly that.
- The **proxy** holds the key continuously at `operating` — an invariant, because "objects landing in
  object storage are encrypted" has to keep being true.
- The **assertion holds no custody at all.** A HEAD carrying no SSE-C header returns 400 for an
  encrypted object and 200 for a plaintext one, so the check needs no key. The read-only ceiling pays
  for itself: the gate that proves the key works cannot itself hold the key.

**Two more shared packages, both found the same way.** `internal/harborauth` (the registry-auth
client — robot credentials, bearer challenge, scope claims) because the encryption gate proves the CA
chain by making *Harbor* write a blob, so it authenticates as Harbor does. And `internal/s3sig` (the
SigV4 chain and endpoint arithmetic) because the SSE-C probe signs its own HEADs. Both were private
to a single file and called by four others through proximity.

**And a fixture lesson worth keeping.** `testDeps`'s `SecretField` started as a no-op returning `""`,
which sent every secret-reading case down the "credential missing" branch — the assertions still ran,
against nothing. Giving the fixture the *real* base64 decode is what made them mean anything. Same
shape as the `Summary` stub in teardown: a seam stubbed to do nothing turns a test into a tautology.

### What `guard-charts` showed — a routine one, and the library it dragged out

The fourth gate, adding no new shape. It was taken now precisely for that: after nine extractions
that each found something, the useful question is whether a routine extraction is routine **yet**.

**It was.** One shape, no ceiling probe, no vocabulary argument, the smallest `Deps` in the repo —
a single `GitOutput`. That is what a gate looks like when the pattern has settled: a gate reaches
nothing, so the only capability it cannot supply itself is asking git what *changed*, which is the
one question `chart-version-guard` is about.

**Three checks, one binding.** `reconcile-actions` split into four named invariants because their
grants differ; these three hold the same grant at the same moment, so naming them separately would
buy nothing their own names do not already say. **The split is justified by divergent capability, not
by count** — worth stating, because "one binding per check" is the obvious wrong generalisation from
the reconciler.

**And the seventh shared package, which is the real yield.** `internal/guardwalk` — find the YAML
under a set of roots, decode the documents that matter, sort the findings — is called by **ten**
guards. The catalog named `guard_walk.go` as one of the "shared libraries every extension links" and
said it belongs in `tools/internal/*`; the count made the argument by itself once a fourth guard
needed it. Every remaining `guard-*` and `assert-*` extraction now starts with that already done.

`SortFindings` came with it, and is worth not losing: output stability is a **correctness** property
for a guard, not a nicety. Findings that reorder between runs produce a diff on every CI run, and a
gate whose output always differs is a gate people stop reading.

## The cost of the interesting half

Three extensions in, the model is exercised by two kinds (`gate`, `invariant`), two states
(`scaffolded`, `operating`) and one grant (`read-repo`). The untested half is not untested by
accident, and the reason is measurable.

Counting the package-`main` symbols each candidate's files reference — its **closure**, not its file
list:

| candidate | shape it exercises | coupling to package `main` | extracted? |
|---|---|---:|:-:|
| `guard-budgets` | `gate` · `read-repo` | 0 | ✅ |
| `guard-docs` | `gate` · `read-repo` | 2 | ✅ |
| `posture-at-rest` | **`invariant`** · `read-repo` | 2 | ✅ |
| `assert-storage` | **`assertion`** + `invariant` ×2 · **`cloud-mutate`** · named | 16 → 4 seams | ✅ |
| `template-sustain` | `assertion` + `gate` · `upgraded` | 26 → **4 seams** | ◐ 4 of 7 files |
| `teardown` | **`transition`** + `assertion` · `destroyed` · `cloud-mutate` | 30 → **7 seams** | ✅ |
| `import-brownfield` | **2× `transition`** · **`own-paths`** · **opt-in** | 24 → **11 seams** | ✅ |
| `obj-encryption` | **`transition:seeded`** · **`secret-custody`** · 3 bindings | 43 → **25 → 5 seams** | ✅ |
| `reconcile-actions` | **`invariant` ×4 · two grant sets · `secret-custody`** | 62 → **28 → 4 lanes moved** | ◐ |

**The raw counts were too pessimistic, and I can now say by how much.** `assert-storage`: 16 raw
references collapsed to 4 injected seams, because most were one-line wrappers around the same four
capabilities. `reconcile-actions`: the census said 62, but that number counted identifiers inside
**comments** — the word "reconciler" appears constantly in prose. Re-measured with comments and
string literals stripped, the eight lanes reference 28 symbols, and four of the lanes turned out to
need almost nothing because a lane is a *free function taking a client interface*, not a method on
the runtime type.

So: treat the raw counts as an upper bound, and re-measure properly before concluding a candidate is
out of reach. The corrected method is comments-stripped closure over non-test sources. What did NOT
change is the shape of the finding — the lanes that stayed are still the ones with the expensive
dependencies, and the four seams `assert-storage` needed are still the same four the next candidates
need.

**The catalog's line counts are file counts, not closures**, and the gap between the two is where the
remaining work is. The cheap extractions are all gates because a gate reaches nothing; every
candidate that would exercise a mutating grant is, by construction, one that holds a credential
handle, a cluster client or a cloud client — and those live in package `main`.

Three consequences, and the first two change the plan:

- **The suggested order below is wrong.** It ranks by lines relieved, which put `converge` second and
  `import-brownfield` third. Closure says those are among the most expensive things available, and
  that the next tranche of *cheap* relief is the six remaining gates — which buy no new model
  coverage at all. Size and difficulty are close to uncorrelated here.
- **The action ABI has to come before the expensive extractions, not after.** Every high-coupling
  candidate is coupled through the same four things: a cluster client, a cloud client, a credential
  handle, a kubectl seam. `volumes.Deps` is that list, written down — the ABI's requirements
  document, arrived at by extracting something that needs them rather than by design. A binding's
  declared grants should eventually decide which fields are populated, so a capability the extension
  was not granted arrives nil.
- **`template-sustain` cannot be separated from what defines its grant.** It is the extension that
  would hold `own-paths`, and 26 of its references are into `ci_template_manifest.go` — which ADR
  0014's one-ownership-authority corollary pins as permanently core. The pairing pattern's two halves
  have different extraction costs, and this pair may not be separable at all.

One further extraction (`template-sustain`) was attempted and abandoned rather than forced: 26 of its
references are into `ci_template_manifest.go`, which ADR 0014's one-ownership-authority corollary
pins as permanently core. The extension that would hold `own-paths` may not be separable from the
thing that defines it.

### What none of them proves

Nothing is loaded, dispatched or disabled through the model. All six still run because `ci.go` and
the reconciler register them, and every declaration is inert.

**Now exercised:** all four binding kinds (`gate`, `assertion`, `invariant`, `transition`);
multi-binding extensions and named bindings, up to four on one extension; six of seven grants
(`read-repo`, `cluster-read`, `cluster-write`, `cloud-read`, `cloud-mutate`, `secret-custody`); four
states (`scaffolded`, `verified`, `operating`, `destroyed`); and `grantStates` in both directions —
one row wrong and corrected, three right and serving as controls.

**Nothing in the vocabulary is unexercised any more**, and `seeded` — the state the old ceiling banned
by omission — is now occupied. All four binding kinds, all seven grants, both values of `Always`,
multi-binding extensions, named bindings and `Incomplete` are declared against real code, across five
of the ten states. `configured`, `converged` and `promoted` remain untouched, and belong to candidates
in the lower half of this table.

**And three things the model cannot express**, all found by declaring rather than by design:

1. a binding that **writes repository files** — no `write-repo`, and `own-paths` is a copier fence
   explicitly *not* a write permit (`llz ci gen-toc`, `promote-pipeline`);
2. ~~an extension that is **partial**~~ — **FIXED.** Two independent cases (`reconcile-actions`,
   `template-sustain`) met the bar, and `Extension.Incomplete` landed with them;
3. the difference between **granted and confirmed** — `cloud-mutate` permits a deletion; nothing says
   a human authorised *this* one (`teardown.Deps.Confirm`).

None is invented here. (1) and (2) each wait for a second independent case; (3) is a question for the
action ABI rather than a missing grant.

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

> **Superseded by measurement.** This ranks by lines relieved. [The cost of the interesting
> half](#the-cost-of-the-interesting-half) measures each candidate's *closure* into package `main`
> and finds size and difficulty close to uncorrelated: `converge` and `import-brownfield`, ranked 2nd
> and 3rd here, are among the most expensive things available. Kept as written, because it is what
> the catalog concluded before anything had been extracted, and the correction is worth more than the
> tidy version.

1. ~~**`guard-budgets`** (907) — self-hosting proof, zero grants beyond `read-repo`, already unit-tested.~~
   **Done** — [The first ten, extracted](#the-first-ten-extracted).
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
