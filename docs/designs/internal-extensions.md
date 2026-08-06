# Design: the internal-extension catalog — every file in package `main`, assigned once

**Status:** **Proposed** — analysed, not built. Nothing in the tree depends on this catalog; it is
planning input for [ADR 0014](../adr/0014-core-surface-budget.md) (the core-surface budget) and
issue #10 / issue #399 (the extension framework), and not a commitment to a final extension list.
What it *is* is a measurement: what decomposition is available, and in what order.

**Measured:** 2026-08-03, against `feat/core-surface-budget` (214 non-test files, 41,709 logic
lines). Line counts are `llz ci core-surface --verbose`, so they are the same numbers the budget
gate enforces and can be re-derived at any time. Spot-checked on merge: `health.go` 1,097,
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
  a hundred lines added to `health.go` is not. `exact: true` is what makes that visible at all.

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
- **`cigate.go` + `guard_walk.go` + `kubectl_probe.go`** are the shared libraries every extension
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
| `token-inventory` | 1,473 | 6 | ✔ | ✘ | `tokens` 437, `ci_token_inventory` 330, `token_validate` 211, `ci_rotation_plan` 216, `token_capability` 167, `ci_validate_tokens` 112. **Wants splitting** — it contributes predicates at three states (`configured`, `seeded`, `operating`). Best single candidate for fine-graining. **✅ Extracted — five files, not six.** `tokens.go` is the credential PROVISIONING wizard and alone TRIPLED the closure. See [What `token-inventory` broke](#what-token-inventory-broke--one-word-doing-two-jobs).|
| `env-topology` | 740 | 4 | ✔ | ✘ | **✅ Extracted — three of four files.** `branchpolicy` stayed: its GitHub PUT is `cloud-mutate` at `configured`, which the ceiling refuses. See [What `env-topology` refused to invent](#what-env-topology-refused-to-invent--one-case-is-not-two). `topology` 245, `env_set` 219, `branchpolicy` 165, `envlist` 111. |
| `config-readiness` | 733 | 3 | ✔ | ✘ | **✅ Extracted.** `readiness` 255, `state` 242, `ci_preflight` 236. **This is the `configured` predicate** — the cleanest existing example of predicate code that's mis-filed as a command. |

## `→ provisioned` — grants: `cloud-mutate`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `import-brownfield` | 3,133 | 8 | ✘ | ✔ | The single biggest movable block, and the only large one that is genuinely optional. Externalisable — its four non-trivial core calls become `llz render` / `llz new` argv. **Two bindings, not one:** import *writes an instance repo* (`transition:scaffolded[read-repo, cloud-read, own-paths]`) and *adopts cloud substrate* (`transition:provisioned[cloud-mutate]`). An earlier draft declared it as one transition to `provisioned` holding `own-paths`, which the validator rejects — own-paths is only meaningful where files are written.  **✅ Extracted** — the first opt-in, and `ext? ✔` was WRONG; see [The first ten, extracted](#the-first-ten-extracted).|
| `cluster-bootstrap` | 964 | 2 | ✔ | ✘ | `ci_bootstrap_cluster` 771 + manifests 193. ADR 0011's payload. |
| `cluster-access` | 952 | 4 | ✔ | ✘ | `runner_acl` 458, `runner_acl_configmap` 201, `fetchkubeconfig_state` 192, `fetchkubeconfig` 101. **✅ Extracted** — and the header above is now wrong for it: this state's grants are not `cloud-mutate` alone. See [What `cluster-access` found](#what-cluster-access-found--the-credential-the-table-forgot).|
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
| `converge` | 1,599 | 5 | ✔ | ✘ | `ci_health` 1,097, `ci_wait` 216, `statushealth` 114, `wait_apl_pipeline` 96, `nudge_argo` 76. **The acid test. ✅ Extracted — the prediction held exactly; see [What `converge` settled](#what-converge-settled--the-acid-test-and-what-it-did-not-break).** The action/assertion split it needs is already built and can be copied rather than invented: `internal/health` is 1,164 lines of pure classification that `health.go`'s header calls "the tested internal/health predicate", with the command reduced to the kubectl orchestration feeding it. That library half is an `assertion:converged`; the command half is the `transition:converged`. |
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
| `assert-network` | 840 | 4 | ✔ | **✅ Extracted.** network-enforcement 440, admission-enforcement 240, net-probe 83, wave-health-vap 77. See [What `assert-network` corrected](#what-assert-network-corrected--a-name-that-read-like-a-capability). |
| `assert-reconciler` | 725 | 2 | ✘ | 433 + effects 292 — pairs with `reconciler-runtime`. **✅ Extracted** — 1,044 lines, not 725. See [What `assert-reconciler` decided](#what-assert-reconciler-decided--the-pairing-question).|
| `assert-storage` | 631 | 3 | ✔ | volume-encryption 265, reconcile-volume-tags 203, relabel-volumes 163 (holds `cloud-mutate` — the odd one out). **✅ Extracted** — the flag was a defect report, not a footnote; see [The first ten, extracted](#the-first-ten-extracted). |
| `assert-identity` | 627 | 2 | ✔ | team-login-smoke 469, certificates 158 |
| `assert-platform` | 602 | 5 | ✔ | health-workflow 210, argo-app 130, instance-custom 106, image-fresh 82, apl-version 74. **✅ Extracted — four of five files.** `image-fresh` is template-pin machinery and stayed. See [What `assert-platform` showed](#what-assert-platform-showed--the-first-extension-that-only-looks).|
| `assert-objstore` | 560 | 3 | ✘ | obj-roundtrip 307, `s3_object` 131, `s3_probe` 122 |
| `assert-registry` | 381 | 1 | ✘ | harbor-roundtrip — pairs with `harbor-provisioner`. **✅ Extracted** — closure **2**, the cleanest boundary of all seventeen. See [What `assert-registry` cost](#what-assert-registry-cost--nothing-and-that-is-the-finding).|
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
| `posture-credential-coverage` | 664 | 2 | ✔ | ✔ | `ci_extsecret_paths` 456, `ci_credential_coverage_guard` 208. **✅ Extracted — and it is a GATE, not an invariant.** It reaches no cluster. See [What `posture-credential-coverage` corrected](#what-posture-credential-coverage-corrected--the-first-wrong-state).|
| `reconcile-actions` | 648 | 7 | ✔ | ✘ | es-store-recovery 141, openbao 135, tokens 116, apl-overlay 106, argo-nudge 81, sc-demote 39, linode-token-wait 30. **Seven separate invariants** — the clearest case for one-invariant-per-extension. **◐ Four of eight extracted** — and `linode-token-wait` is not a lane at all; see [The first ten, extracted](#the-first-ten-extracted). |
| `posture-plaintext` | 626 | 1 | ✔ | ✔ | The largest single guard and the most instance-tunable (its protocol allow-list is policy, not fact). Best stress test of the vehicle. |
| `health-sla` | 405 | 3 | ✔ | ✔ | sla 165, readiness 162, incluster 78. **✅ Extracted — but only two of the three files.** `incluster` is part of `converge`, not this; grouping by filename prefix grouped it wrong. See [What `health-sla` corrected](#what-health-sla-corrected--a-catalog-row-that-grouped-by-filename).|
| `posture-mesh` | 364 | 2 | ✘ | ✔ | mtls-wiring 211, mesh-egress 153 |
| `posture-at-rest` | 304 | 1 | ✔ | ✔ | **✅ Extracted** — the first non-gate binding; see [The first ten, extracted](#the-first-ten-extracted). |
| `wave-health` | 178 | 1 | ✔ | ✔ | |

## `→ promoted` / `→ upgraded` / `→ destroyed`

| extension | lines | files | always | ext? | notes |
|---|---:|---:|:-:|:-:|---|
| `release-publish` | 1,150 | 5 | ✘ | ✘ | chart-publish-check 366, `gh_gitdata_native` 239, pin-images 204, publish-charts 187, deliver-docs 154. Template-repo-side, not instance-side. |
| `teardown` | 1,070 | 4 | ✔ | ✘ | `ci_teardown` 492, `reap` 328, destroy-unwedge 207, crd-unwedge 43 **✅ Extracted** — the first transition; `reap` and `drain-obj-buckets` stayed. See [The first ten, extracted](#the-first-ten-extracted). |
| `template-sustain` | 630 | 5 | ✔ | ✘ | `upgrade_policy` 236, `drift` 114, `template_removals` 94, `upgrade_churn_guard` 107, `stamp` 79. Consumes the `own-paths` grant. **◐ Partial** — the own-paths half cannot leave core (ADR 0014). See [The first ten, extracted](#the-first-ten-extracted). |
| `promote-pipeline` | 307 | 2 | ✔ | ✘ | **✅ Extracted — binds `promoted`, the last unclaimed state.** See [What `promote-pipeline` closed](#what-promote-pipeline-closed--the-last-state-and-the-third-write-repo-case). `promote_gen` 173, `promotion` 134. Already a codegen DAG — same shape as `extension_ci.go`; **the two should share one emitter**. Grants `read-repo` only: its output `promote.yml` is a copier-rendered `merge` stub, so it does *not* want `own-paths` (see Decision 1). |

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

## The first twenty-two, extracted

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
| `guard-charts` extracted | 38,364 | 202 | −457, and `guardwalk` — the traversal ten guards share |
| `cluster-access` extracted | 37,483 | 199 | −881, and the second `grantStates` widening — see below |
| `health-sla` extracted | 37,131 | 197 | −352 only — plus `kubectlprobe`, the probe **ten** callers share |
| `token-inventory` extracted | 36,107 | 192 | −1,024, the first `configured` binding — and the first new **word** in the model |
| `converge` extracted | 34,359 | 188 | **−1,748** — the acid test, plus `cigate` (12 callers) |
| `assert-platform` extracted | 33,877 | 185 | −482 — the first PURELY-assertion extension |
| `assert-reconciler` extracted | 33,157 | 185 | −720 — the second OPT-IN, plus `promwire` |
| `assert-registry` extracted | 32,965 | 185 | −192 — the cheapest, and the only one needing NO `Deps` |
| `promote-pipeline` extracted | 32,733 | 184 | −232 — binds `promoted`, **the last unclaimed state** |
| `posture-credential-coverage` extracted | 32,077 | 182 | −656 at 90.5% coverage — a GATE the catalog filed as an invariant |
| `config-readiness` extracted | 31,372 | 182 | −705, plus `instancelayout` — a hub extracted to break a **cycle** |
| `env-topology` extracted | 30,687 | 179 | −685, plus `yamledit` — and a binding **removed** rather than a row widened |
| `assert-network` extracted | **29,853** | 176 | −834 — **below 30,000**, and the best ratio yet (closure 6 / 1,267 lines) |

**Net −17,329 (36.7%) across twenty-two extensions**, and now *below* the 41,803 this gate first recorded —
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

### What `cluster-access` found — the credential the table forgot

Eleventh, and the second extraction to change the model rather than fit into it.

```
cluster-access  transition:provisioned [cloud-mutate, cluster-write, secret-custody]
```

**`Validate()` rejected it.** `grantStates` listed `secret-custody` as legal at `seeded` and
`operating` only:

```
secret-custody may only be asked for at seeded, operating — a binding elsewhere that needs it
is either mislabelled or is widening what the state is understood to do
```

The declaration was not wrong. `RunFetch` writes a **cluster-admin kubeconfig** to disk, and
[team-scoped-credentials.md](team-scoped-credentials.md) calls it the one human-facing credential per
cluster. That is custody in the strictest sense the model has.

**What the row had encoded.** `secret-custody` had only ever been shown credentials the platform
*mints* (seeding) or *replaces* (rotation) — both of which happen to a cluster that already works. So
the row quietly meant *"custody begins once there is a platform to hold it"*. The bootstrap
credential breaks that: the **cloud** issues it at provisioning time, and holding it is the
precondition for seeding rather than a consequence of it. A table that cannot express the first
credential in the system's life is describing the middle of the story only.

`provisioned` was added, with the argument in `validate.go` and the pin updated in
`grantstates_internal_test.go`.

**Both widenings were found the same way, at opposite ends.** `cloud-mutate` gained `operating` (the
fourth extension) at the *end* of the lifecycle — reconciler lanes that keep running. This one gained
a state at the *start*. Neither was predicted by reading the catalog; both surfaced only when code
that already ships had to describe itself. That is the argument for extracting the **expensive**
capabilities before trusting the ceiling, not after — and it is now made twice.

**The state header above this extension's catalog row is wrong.** `## → provisioned — grants:
cloud-mutate` was written from the catalog's own reading, and `cluster-access` holds three grants
there. Left in place with a pointer rather than silently corrected: the catalog is evidence of what
was believed before the extractions, and quietly editing it would destroy the record of the model
being wrong.

**And a second trap: extraction renames files, and the secrets guard reads filenames.** The natural
names for this package's files were `kubeconfig.go` and `kubeconfig_state.go`. The pre-commit secrets
guard blocks `(^|/)kubeconfig` — a path segment *starting* with it — so `fetchkubeconfig.go` passed
for years and the tidier name did not. The guard was **right and the rename was wrong**: a file whose
path segment begins `kubeconfig` is exactly the leak vector it exists to catch, and `.go` is not
evidence to the contrary. They are `fetch.go` / `fetch_state.go` instead, which also matches
`acl.go` / `acl_configmap.go`. Narrowing a security guard as a side effect of a refactor would have
been the cheaper fix and the wrong one.

**The trap, for the next extraction that shells out.** `internal/clusteraccess` re-executes the
binary (`<self> render <env> --tfvars-only`). Under `go test`, `os.Executable()` is the *test binary*
— so the moved code re-ran its own suite, which shelled out again, and `go test ./...` did not fail,
it **hung**. `package main` had a `TestMain` guard for exactly this; the extraction moved the code
that shells out and left the guard behind, because a guard wired into one package's `TestMain` is
invisible to the file being moved. Cost: a wall-clock timeout to discover rather than a red test.

### What `health-sla` corrected — a catalog row that grouped by filename

Twelfth, and the first whose finding is about **the catalog** rather than the model.

```
health-sla  invariant:operating "rotation-sla"        [cluster-read, secret-custody]
            invariant:operating "component-readiness" [cluster-read]
```

**The row named three files; only two belong.** The catalog listed `ci_health_sla.go`,
`ci_health_readiness.go` and `incluster.go` as one extension. They share a filename prefix
and nothing else: `health-incluster` computes the **convergence verdict** over `internal/kube` with
the pod ServiceAccount, sharing the exit-code contract and the classifier with `health.go`. It is
part of `converge` and stayed behind for that extraction. **Grouping by name prefix is grouping by
how someone once filed the code** — which is precisely the misfiling ADR 0014 says package `main` is
full of. The catalog inherited the filing it was written to describe.

**The split is the declaration's whole content.** `guard-charts` established that a split must be
justified by *divergent capability* rather than by count; this is the case where divergence actually
shows up. The Loki OBJ-key SLA reads `OPENBAO_ROOT_TOKEN` and execs `bao kv metadata get` with it;
the readiness checks hold no credential at all. One binding would widen grants to the union and hand
the readiness lane secret-custody it never uses.

**A gap the vocabulary cannot yet express.** That check reads only `updated_time` — metadata, never
the secret material. It is nonetheless declared `secret-custody`, because the model judges what a
binding **is handed**, not what it promises to do with it, and this one is handed the token that
opens every secret in the store. But `[cluster-read]` would under-report it and `[secret-custody]`
over-reports it, and there is no third thing to say. **This is the same shape as the open
granted-vs-confirmed gap:** the grant names a capability, and what is missing is an axis for how much
of it is exercised. Two instances now; a third would make it actionable.

**The ninth shared package, and the larger yield.** `internal/kubectlprobe` — the classified
`kubectl get` that distinguishes *"the resource is absent"* from *"we never got an answer"* — had
**ten** non-test callers in package `main`. That is the same threshold at which `guardwalk` was
extracted, and the same argument. Every remaining cluster-facing extraction now starts with it done,
`converge` most of all.

**One seam, not two.** The probes hold their own `Exec`, so `exec.go` now wires it to package main's
in `init()` and `withExecOutput` swaps **both**. Stubbing only one leaves the probes shelling out to
a real cluster — which, as the previous extraction established, is a hang rather than a failure.

### Three traps this one paid for

**Comments are not code, again.** A symbol rename across the probe file rewrote the English word
*"answered"* to *"Answered"* in six prose sentences, because `answered` was also a method name. The
recorded rule is to strip comments and strings before renaming; the working version of that rule is
the small parser now used for the ten call sites, which splits each line at its `//` and stashes
string literals before substituting. It rewrote twelve files with zero comment or string damage.

**Tests that never travelled.** `TestReadyCell` and `TestSchedRegion` lived in
`coverage_tier1_test.go` — a file named for a *coverage tier*, so nothing about it suggested it held
assertions about these checks. The recorded lesson is that tests travel with the **file** rather than
the subject; this is that lesson from the other side, where a test fails to travel because it was
never filed with its subject to begin with. `go vet` found them as undefined symbols. **Grep for the
moved symbols, not only for the moved files.**

**Fixture install order.** The new `Deps` baseline was installed by a helper that ran *after* the
tests had already stubbed their seams, silently wiping them — four tests failed with 12-second
retry stalls that looked like a cluster timeout. Fixed by making installation idempotent
(`ensureDeps`) rather than by documenting the required order: ordering dependence between fixtures is
its own bug class, and a comment does not remove it.

### What `token-inventory` broke — one word doing two jobs

Thirteenth, the **first binding at `configured`** — the last unclaimed state in the vocabulary — and
the first extraction to add a **word** to the model rather than a row to a table.

```
token-inventory  assertion:configured "validate-tokens"  [read-repo, secret-read]
                 gate:configured      "rotation-plan"    [read-repo]
                 invariant:operating  "expiry-inventory" [cloud-read, secret-read]
```

**Measure before trusting the catalog — again, and expensively.** The row named six files and 1,473
lines. `tokens.go` (437 of them) is `llz tokens`, the credential **provisioning** wizard that creates
OBJ buckets and gathers PATs, and it alone took the measured closure from **13 to 42** by dragging in
the wizard, the state model and the command tree. It is a `transition` to `configured` holding
`cloud-mutate`; these are the checks that *read what it wrote*. The catalog's "wants splitting across
three states" was right about the split and wrong about which files were in it.

**The refusal was not the one predicted.** The expectation was a third `grantStates` widening —
`secret-custody` at `configured`, mirroring the eleventh extension's widening to `provisioned`. It
never got that far:

```
gate:configured/validate-tokens[read-repo, secret-custody]: a gate permits only "read-repo",
not "secret-custody" — it runs in the fast pre-commit path over files alone
```

`validate-tokens` probes GitHub, Linode and S3 over the network. It **blocks the pipeline**, which is
what gates do, but a gate in this model is defined by *cost and reach* — fast, local, files only.
Those are two different properties and the single word `gate` was carrying both. The honest kind is
`assertion`, which may bind at any state.

**And an assertion could not hold it either.** `secret-custody` was not a read grant, so it was
refused there too — which left `llz ci validate-tokens` **inexpressible**. Not mis-described, not
over-granted: there was no legal declaration for a check that reads credentials and mutates nothing.

**The word was doing two jobs, and said so.** Its own definition read
`SecretCustody Grant = "secret-custody" // read or write credential material`. Three extractions
pushed on that ambiguity in a row:

| extension | what it does with credentials | |
|---|---|---|
| `cluster-access` | **writes** a cluster-admin kubeconfig to disk | custody |
| `health-sla` | **reads** `updated_time` using the OpenBao root token | declared custody *under protest* |
| `token-inventory` | **reads** every pipeline credential and probes it | impossible |

So the grant was split: **`secret-read`** for reading credential material or its metadata (read-only,
an assertion may hold it) and **`secret-custody`** for placing it (mutating, still bound by
`grantStates`). `health-sla`'s rotation lane was corrected to `secret-read` in the same commit — the
comment there had already recorded that it over-reported, and this is the word it was missing.

**No `grantStates` row was widened, and that is the better outcome.** The ceiling was not too tight;
the vocabulary was too coarse. Widening the row would have "fixed" this by letting every
credential-reading check in the repo claim a mutating grant — which is how a ceiling stops meaning
anything.

**The open gap from §13 is now closed.** `health-sla` recorded that the model could not distinguish
*"handed root, reads metadata"* from *"handed root, reads material"*, and noted two instances with a
third making it actionable. This was the third, and it did not merely under-report — it made a
declaration impossible, which is the difference between a wart and a defect.

**Tests that never travelled, twice more.** `TestCapitalizeFirst` and `TestSecretsWritePATURLRequests-
Environments` both moved with the wrong file — the first from `coverage_tier1_test.go` (a file named
for a *coverage tier*), the second because it rode along with `token_capability_test.go` while its
subject, the wizard's pre-filled PAT link, stayed in package `main`. Third and fourth instances of the
same lesson: **a test's filename says where someone put it, not what it is about.**

### What `converge` settled — the acid test, and what it did NOT break

Fourteenth, and the one the catalog named the acid test two hundred declarations before it was
attempted: *"`ci_health.go` fuses the converge ACTION with the health PREDICATE, and `internal/health`
is the precedent for splitting them."*

```
converge  transition:converged "drive"            [cluster-read, cluster-write]
          assertion:converged  "health"           [cluster-read]
          assertion:converged  "health-incluster" [cluster-read]
```

**The prediction held exactly, and the split was available rather than invented.** `internal/health`
was already 1,164 lines of pure classification; the commands were already the kubectl orchestration
feeding it. The declaration writes down a separation the code had almost made.

**And the split is not cosmetic.** `llz ci converge` polls toward the verdict *and acts on what it
sees*: it patches Argo Applications, and it strips oversized `last-applied-configuration` annotations
off CRDs when a sync hits the 256KB limit. Those are cluster writes performed from inside what reads
like a health check — a reviewer running `llz ci health --wait` would reasonably assume it observes.
The grant line is the correction, and **nothing said this anywhere before the declaration existed**.

**No ceiling change.** `cluster-write` was already legal at `converged` — that row exists for exactly
this transition. The acid test did not break the model.

**What it strained instead was the ACTION ABI's absence.** Every extension before this takes `Deps`
as a *parameter*. `converge` is 2,476 lines whose call tree runs six or seven frames from the entry
point to the leaf that shells out, and the leaves are the health sections — small predicates whose
entire content is a classification. Threading a capability argument through forty of them is the
opposite of what the split is for, so the seams are package-level and installed once
(`converge.Install`). That is what they already were in package `main`; the change is that they are
now *named* as a capability set rather than ambient.

The cost is real and is stated in `deps.go`: an installed seam is global mutable state, tests must
restore it, and two callers cannot hold different `Deps` at once. Nothing needs that today — one
binary, one cluster per invocation — **but "nothing needs it today" is the sentence that precedes an
action ABI.** An ABI would hand each binding its own handle at dispatch, which is precisely what
package-level installation cannot do. `cmd/llz/ci_converge.go` is the hand-written version of that
dispatch, and it is the strongest argument for the real thing that these fourteen extractions have
produced.

**The one rule this extraction broke deliberately.** Every other extension lifts its cobra commands
back to package `main`; `converge` exports its own constructors. The rule exists so the CLI's shape
stays in the CLI, and it buys nothing here: the seven verbs share one flag vocabulary and one
exit-code contract, and transcribing them would move ~280 lines of boilerplate across the boundary
while leaving every decision they encode on the other side of it. What package `main` owns here is
the capability set — which is what `ci_converge.go` now is.

**Two seams were drawn in the wrong place, and the tests said so.** `LokiConfigText` started as a
`Deps` field; it is a plain classified ConfigMap read, and `converge` already holds `cluster-read`.
Injecting it meant the package needed permission to do what it was already permitted to do — and the
fixture that resulted returned `""` for every test, so the S3-detection assertions ran against
nothing. **A seam in the wrong place does not merely add indirection; it manufactures a vacuous
fixture.** The second was `Summary`, defaulted to a no-op: a test asserting that an unwritable summary
path *warns* can never pass against a default that cannot fail. Both are the vacuous-fixture bug two
earlier extractions shipped — and the new lesson is that **an installed default is a fixture too**.

**The tenth shared package.** `internal/cigate` — the kubectl/clock seam, the deadline poll loop
every gate spells the same way, the kubeconfig tempfile spill — had **twelve** non-test callers.
`readRegionTFVars` and `ciClient` did not come along: they know the repo's terraform layout and the
CI PAT reader, and a shared package that knew either would be a shared package that knows this
repo's shape.

**The `TestMain` trap, for the second time and in a new form.** `internal/kubectlprobe` retries an
unanswerable kubectl call three times with a 3s gap, and package `main` zeroed that delay in its own
`TestMain`. The line did not travel, so every converge test paid **six real seconds** — the suite took
**568 seconds** and then began tripping CI's 300s timeout outright. `cluster-access` lost the *re-exec*
half of the same `TestMain` and hung. **Any extraction that moves code touching `kubectlprobe` needs
that line**, and the general shape is worth stating plainly: a guard wired into one package's
`TestMain` is invisible to the files being moved.

### What `assert-platform` showed — the first extension that only looks

Fifteenth, and the first whose declaration holds **no mutating grant at all**: four lanes, one read
grant each.

```
assert-platform  assertion:verified   "health-workflow" [cluster-read]
                 assertion:verified   "argo-app"        [cluster-read]
                 assertion:verified   "instance-custom" [cluster-read]
                 assertion:configured "apl-version"     [read-repo]
```

Worth having one of these on the record now that every mutating shape is exercised. It validated with
no ceiling change and no argument — which, fifteen extractions in, is the point of running it.

**The catalog named five files; four belong.** `ci_assert_image_fresh.go` stayed in package `main`:
its closure is the **template-pin machinery** (`assertPinCoherence`, `pinnedTemplateRef`,
`resolveTemplateCommit`), and it asserts that an instance's pinned template ref and its images agree —
a `template-sustain` question wearing an `assert-` filename. **Fourth time the catalog's file list has
been wrong, and the fourth time for the same reason: it grouped by name.** The running score is
`guard-docs`, `health-sla`, `token-inventory`, `assert-platform`.

**One lane binds a different state, and that is the interesting part.** Three of these read a cluster.
`assert-apl-version` does not — it reads the pinned apl-core chart version out of the **spec file** and
compares it against the floor this llz supports, deliberately before anything is provisioned. It is
the same shape `token-inventory`'s `validate-tokens` lane established: **a preflight is not a gate just
because it blocks.** It reads more than files, so it is an assertion; it reads them before
provisioning, so it binds `configured`.

**A duplicate the extraction surfaced, and did NOT merge.** `internal/clusterspec` already had
`aplSemver`, and package `main` has `semver` in `selfupdate.go`. They look identical and are not: one
strips a leading `llz/` because it parses llz **release tags**, the other `TrimSpace`s and rejects
negatives because it parses operator-typed **chart versions** out of a spec file. Same shape, different
inputs — collapsing them would make each wrong for the other's source. `AplSemver`/`AplSemverLess` were
exported from `clusterspec` instead, and the reason not to merge is recorded next to them.

**A test that has now moved twice.** `TestImportScaffoldsASupportedChart` compares the chart version
`llz import` scaffolds against the floor `assert-apl-version` enforces. Its own comment said it lived
in package `main` because that was "the side that owns both halves" — which stopped being true the
moment this extension was extracted. It moved, and the comment was corrected rather than left to rot.
**A comment explaining why code lives somewhere is a claim that expires when the code moves.**

### What `assert-reconciler` decided — the pairing question

Sixteenth, the **second opt-in extension** (`import-brownfield` was the first), and the first half of
one of the catalog's four capability/assertion pairs to land.

```
assert-reconciler  assertion:operating "functional-health" [cluster-read]
                   assertion:operating "effects"           [cluster-read]
```

**It answers a question the catalog left open.** The catalog observed that `reconciler-runtime` ↔
`assert-reconciler`, `harbor-provisioner` ↔ `assert-registry` and two more pairs "turn on and off
together", and suggested **one extension carrying both bindings** might beat two — *"worth deciding
early, because it halves the count"*.

**After extracting this half: they should stay separate.** `reconciler-runtime` will hold
`cluster-write`, `secret-custody` and a leader election; this holds `cluster-read` and nothing else.
One declaration would have a grant line that is the **union** — the over-granting per-binding grants
exist to prevent — and the read-only assertion could no longer be reasoned about apart from the
process it judges. **The pairing is real, but it is an ENABLEMENT relationship, not an identity.**
Whatever the manifest ends up doing about co-enablement, it should not be done by merging.

**Assertions at `operating`, which is the distinction most easily lost here.** `reconcile-actions`
holds *invariants* at the same state. An invariant is a property an extension **maintains**; these
only observe. Same state, opposite side of the fence — and if a lane here ever starts repairing what
it finds, that half belongs in the runtime extension.

**A seam in the wrong place, for the second extraction running.** `leaseHolderRenew` started as a
`Deps` field. It is not a capability — it reaches nothing, it is a pure function over a decoded
object — and the tests said so immediately: a capability default has to do *something* harmless, so it
returned `("", zero, false)`, and every lease-freshness assertion ran against a parser that never
parsed. It now lives in `internal/kube`, shared with the reconciler that **writes** the Lease this
extension **judges** — which matters, because the `renewOK` contract encodes a real incident (an
unreadable timestamp reading as "lease is free", so a live holder's lease gets stolen and two
reconcilers run every write lane at once).

`internal/converge` learned the identical lesson one extraction earlier with `LokiConfigText`. Stated
once more, because twice is a pattern: **ask whether the package can already do this with a grant it
holds. If yes, it is not a seam.**

**The eleventh shared package.** `internal/promwire` — Prometheus instant-query decoding — is shared
by this extension and `assert-rotation-health`, and `assert-observability` is built entirely on it.
The property it protects is the same one `internal/kubectlprobe` protects for kubectl: **a query
failure and an empty result are different answers.** "We could not ask" reading as "the lane is dead"
sends an operator after a healthy reconciler.

**A guard that now points across the boundary.** `TestReconcileFlagLaneTableMatchesReconcileGo` reads
`reconcile.go` to check that the flag names and lane names have not drifted apart. That file is still
package `main`'s, so the test reads it at `../../cmd/llz/reconcile.go` — and when `reconciler-runtime`
is extracted, a loud failure here is the **correct** outcome. A coupling guard that silently stops
finding its subject is worse than one that breaks.

### What `assert-registry` cost — nothing, and that is the finding

Seventeenth, the **cheapest extraction of all of them**, and the only one that needed **no injected
capabilities at all**.

```
assert-registry  assertion:verified "harbor-roundtrip" [cluster-read, secret-read]
```

It measured a **closure of 2**, both entries noise. Everything it does is an OCI distribution v2
handshake over `net/http`, and the one cluster read it needs already had a home *and a seam* in
`internal/harborauth`.

**A `Deps` struct was written for it and then deleted.** The draft carried `ReadSecret` — duplicating
`harborauth.ReadRobotSecret`, which is already a swappable package var the existing tests drive — and
a `Now`/`Sleep` clock pair for a settle loop those same tests already exercise with millisecond
budgets. Both were seams for capabilities that were **already seamed**.

**That is three extractions running.** `converge`'s `LokiConfigText` was a plain cluster read the
package could already do; `assert-reconciler`'s `leaseHolderRenew` was a pure function; this one's
fields were redundant with an existing seam. The rule has earned a third clause:

> Before adding a `Deps` field, ask three things. Can the package already do this with a grant it
> holds? Is it a pure function rather than a capability? **Is it already injectable somewhere else?**
> A redundant seam is not free — it splits one swap point into two, and lets a test stub half of it.

**It is also the first extension declared *after* the `secret-read` split that needs the read half.**
`assert-registry` reads the robot credential Secret and logs in with it. Under the single
`secret-custody` word this binding would have been **inexpressible** for exactly the reason
`validate-tokens` was: an assertion permits read grants only, and `secret-custody` was half a write
grant. Two extractions later the vocabulary absorbs it without comment, which is what a good fix looks
like.

**The grant is the scar.** Managed instances once rendered `HARBOR_HOST` as `"harbor."` — non-empty,
so it defeated every empty-string guard including the systeminfo fallback — and every push and pull
401'd. Every credential in the chain was valid; the **host** was wrong. Nothing caught it because
nothing ever *used* the credential: the provisioner asserted it had **created** a robot, not that the
robot could **log in**. An assertion holding only `cluster-read` could not have caught that. It has to
hold the credential and try — which is precisely what `secret-read` on this binding declares.

**Second half-pair to land, and a cleaner illustration than the first.** `assert-reconciler` settled
that capability/assertion pairs should not merge; this shows why more sharply. `harbor-provisioner`
will hold `cloud-mutate` and `secret-custody` to **mint** the robot; this holds `cluster-read` and
`secret-read` to **use** it. Nothing in a merged union would be true of either half.

### What `promote-pipeline` closed — the last state, and the third `write-repo` case

Eighteenth, and it binds **`promoted`** — the last state in the vocabulary that nothing claimed. **All
ten lifecycle states now carry at least one binding.**

```
promote-pipeline  transition:promoted[read-repo]
```

**The grant line is true because the file split was made to keep it true.** This extension's whole
output is a file: it writes `.github/workflows/promote.yml`. `read-repo` does not say so. Both obvious
fixes fail:

- **`own-paths` is the nearest-looking grant and the wrong one.** Per [Decision 1](#1-generated-files-own-paths-is-a-fence-against-copier-not-a-claim-on-authorship)
  it means *"copier must not render these bytes"* — a fence, not a write permit — and `promote.yml` is
  a copier-rendered `merge` stub, so the fence would be factually wrong too. `Validate()` rejects it
  regardless, and said so when probed:
  ```
  transition:promoted[read-repo, own-paths]: "own-paths" is only meaningful on a transition to
  "scaffolded" or "upgraded" — it declares files the template must not re-render (ADR 0014)
  ```
- **Inventing `write-repo` was the other option, and was deliberately not taken.**

**Why not invent it.** This catalog already reached the identical gap for `llz ci gen-toc` and wrote
down the rule: *"two independent cases is enough to say the vocabulary has a hole and not enough to
know its shape, so nothing was invented — the file split follows the declaration instead."*
`guard-docs` resolved it that way. **This is the third case and it resolved the same way**, which is
evidence the split is a real answer rather than a workaround being repeated.

Contrast `secret-read`, which *was* invented two extractions earlier. The difference is the failure
mode: `secret-custody` made `validate-tokens` **inexpressible**, so the model had to grow. Here
`read-repo` validates fine — it merely under-reports, and a file split fixes that without touching the
vocabulary. **A gap that makes a declaration impossible is a different thing from one that makes it
incomplete**, and only the first justifies a new word.

**The count for whoever does decide the grant's shape is now much larger than three.** Sixteen
non-test files in package `main` call `os.WriteFile`. Those are `write-repo`'s candidates, and the
question it has to answer is which of them write the **operator's repo** (this one, `gen-toc`) versus a
build artifact or a temp file — a distinction none of the three cases so far has had to draw.

So: rendering lives in the package as `PlanWorkflow`, returning the content and whether it differs;
the `os.WriteFile` lives in `cmd/llz`. `TestPackageContainsNoWritePath` fails if that stops being
true — the same check `internal/docsguard` runs, copied rather than shared, because two is a copy and
three is a library.

**A trap paid for again, by me, in the file I had already written the rule about.** Rebuilding a test
helper, I wrote `open(p,'w').write(open(p).read() + …)` — which truncates before the inner read runs
and destroyed the file. That exact line is recorded as a hard rule in the extraction notes from three
sessions ago. Knowing a trap is not the same as not falling into it; the mitigation that works is the
two-step `with open(...)` form, not the memory of having been bitten.

### What `posture-credential-coverage` corrected — the first wrong STATE

Nineteenth, and the fifth catalog correction — but the first about a **state** rather than a file list.

```
posture-credential-coverage  gate:scaffolded[read-repo]
```

The catalog files it under **`invariant: operating`**, in a section headed *"the binding the current
design has no room for; without it these 4,283 lines stay core-special."* It is neither an invariant
nor at `operating`. **It reaches no cluster and no cloud** — both checks are file scans over the
repo's manifests, and the only I/O in the package is `os.ReadFile`.

**Why this error is more interesting than the previous four.** Those were all *"these files do not
belong together"*. This one groups the right files and puts them at the wrong **moment** — and the
state is what tells a reader WHEN a check runs. Filed at `operating` it reads as continuous
drift-detection against a live platform; it is a pre-commit file check that needs no platform to
exist. A reader planning where to wire it would have reached the opposite conclusion.

**A gate is the strictest claim in the model**, so it is checked rather than asserted:
`TestPackageStaysFilesOnly` fails if the package ever grows a cluster client, a network call or a
write path. `token-inventory`'s `validate-tokens` looked like a gate and was not — it probes GitHub,
Linode and S3 — so the distinction is live, not theoretical.

**Two checks, one binding**, per the `guard-charts` rule: a split needs divergent *capability*, not
divergent subject matter. These are the two ends of one question — does every ExternalSecret path
resolve, and is every credential measured by something.

### The extraction that made four other packages testable

`PlatformTreeDirs` came out with it into `internal/guardwalk` (seven callers), and writing its **first
direct test** found the function's own header comment saying it returns *"the two shared manifest
roots"* while the body had returned **three** since `manifest-secret-store` was added. The body even
carries the scar note explaining that addition. The header was never updated with the fix it
documents.

That triggered a wider repair. Four shared packages had drifted below their coverage floors, all from
the same cause: **helpers were moved into them without their tests**, so each new symbol was exercised
only incidentally through callers.

| package | was | now | the symbol that arrived untested |
|---|---:|---:|---|
| `internal/kube` | 78.0 | **88.1** | `LeaseHolderRenew` |
| `internal/cigate` | 16.9 | **25.4** | `SplitCSVList` |
| `internal/kubectlprobe` | 63.0 | **66.7** | `Reachable` |
| `internal/guardwalk` | 42.0 | **46.0** | `PlatformTreeDirs` |
| `internal/clusterspec` | 95.0 | **96.0** | `AplSemverLess` |

Each now has a test pinning the property that actually matters, not just the line count —
`LeaseHolderRenew`'s is the one worth reading: a renewTime that is **present but unusable** must not
read as **absent**, because the caller treats a zero renewTime as "takeable now". An unreadable
timestamp reading as *"lease is free"* got a live holder's lease stolen once, and two reconcilers then
ran every write lane at the same time.

**The general lesson: a shared package accumulates symbols faster than it accumulates tests**, because
each one arrives as a side effect of an extraction whose attention is elsewhere. The floors caught it,
which is the argument for having them — but only two extractions late.

### What `config-readiness` needed first — a hub extracted to break a cycle

Twentieth, and the catalog called this one **right**: *"this is the `configured` predicate — the
cleanest existing example of predicate code that's mis-filed as a command."* It is, and it needed no
correction. Twenty extractions in, that is worth noting on its own.

```
config-readiness  assertion:configured[read-repo, cloud-read, secret-read]
```

An **assertion**, not a gate: it reads the repo, but it also asks GitHub which secrets and variables
are set and Linode whether the account is reachable. The moment a check leaves the filesystem it stops
being a gate — the same line `token-inventory` and `assert-platform` drew.

**But it could not be extracted alone.** `scaffold-instance` (closure 38), `env-topology` (21) and
`config-readiness` (18) were **circularly entangled**: each reached into `scaffold.go` for
`instanceLayout` and friends, and `scaffold.go` reached back into `readiness.go` and `topology.go`. No
ordering of the three made any of them cheap.

**`internal/instancelayout` is the first shared package extracted to break a cycle rather than on
caller count.** Every previous one — `guardwalk` at ten callers, `cigate` at twelve, `kubectlprobe` at
ten — came out because a threshold was crossed. This one came out because the dependency graph
demanded it. The measured effect:

| candidate | closure before | after |
|---|---:|---:|
| `env-topology` | 21 | **6** |
| `config-readiness` | 18 | **14** |
| `scaffold-instance` | 38 | 38 |

`scaffold-instance` is unchanged, and the reason is worth stating because it corrects a prediction
made before the work: its closure points **outward**, so removing its own symbols from other
candidates' closures does nothing for it. A hub helps the packages that *depend on* it, not the one it
was carved out of.

**Ten symbols were closure candidates; four became `Deps` fields.** Applying the three-clause rule the
previous three extractions paid for — can the package already do this with a grant it holds? is it a
pure function? is it already injectable elsewhere? — removed `orAll`, `validateEnvName`,
`tfvarsValue`, `firstNonEmpty` (pure, localised), `statePassphraseSecret` (a const) and `readEnvFile`
(a file read the package is already permitted to do). **The rule now removes more candidates than it
admits.**

**Two accessors were added rather than exporting fields.** `LiveState` keeps its four maps unexported
because the env→repo fallback in `Has`/`Value` is the whole point of the type; `HasRepoSecret` answers
the one narrower question the wizard genuinely has, and `NewLiveState` lets `llz doctor` construct one
without seeing inside. A caller that can see the maps can also ask questions the type has no answer
for.

### The cost: this was the messiest extraction of the twenty

Thirteen rounds of `go vet` before it was clean, and the errors were nearly all the same two shapes:

- **Over-greedy regexes, three times.** A non-greedy `func Test\w+.*?
}
` match across a file of
  same-shaped functions silently takes the wrong span — it swept four unrelated tests into one move,
  then three more, then a `sustain` test into a `configreadiness` file. The mitigation that finally
  worked was splitting on **line ranges computed from parsed function boundaries**, not on regex.
- **Renaming a common word.** `indent` collided with a local variable in
  `ci_chart_publish_check.go`; `catalog` hit a function definition in `wizard.go`; `f.file` hit four of
  package `main`'s own finding types. Each needed a targeted revert.

Eight tests were found stranded in files named for a **coverage metric** — `coverage_tier1_test.go`,
`branch_coverage_test.go`, `morehelpers_test.go`, `uncovered_helpers_test.go`. That naming is now the
single most reliable predictor of a test that has drifted from its subject.

### What `env-topology` refused to invent — one case is not two

Twenty-first. **A third binding was written and then removed, and that is the finding.**

```
env-topology  transition:configured "env-set"  [read-repo]
              assertion:configured  "topology" [read-repo]
```

`branchpolicy.go` locks the `infra-<env>` GitHub Environment to `main` — a PUT against GitHub's
deployment-branch-policy API, which is a mutation of infrastructure this repo does not contain. The
honest declaration is `transition:configured[read-repo, cloud-mutate]`, and `Validate()` refused it:

```
"cloud-mutate" may only be asked for at provisioned, seeded, converged, operating, destroyed
```

**The row is arguably wrong.** Those five states are where a *Linode* cloud exists to mutate, and the
table was written from a catalog that read `configured` as a purely local moment. But **GitHub is
configured before Linode is provisioned**, and locking a branch policy is exactly the kind of external
mutation a reviewer would want declared.

**It was not widened, because one shipping case is not two.** The bar this branch set for a
`grantStates` row is two independent shipping cases and an argument. There is one. The second is
*predicted* — `llz tokens` creates OBJ buckets and GitHub secrets at configuration time — but
predicted is not shipping, and both rows widened so far (`cloud-mutate`@`operating`,
`secret-custody`@`provisioned`) had code in front of them.

So `branchpolicy.go` went back to package `main`, and this is recorded as **case #1**.
`TestPackageDoesNotMutateExternalInfrastructure` fails if it comes back without the row being argued
first.

**Four times now the answer has been "move the code to the side whose declaration is true"** —
`guard-docs` and `promote-pipeline` for the missing `write-repo` grant, `gen-toc` before them, and now
this for `cloud-mutate`@`configured`. That consistency is itself evidence: **the model's boundaries
are in roughly the right place even where its vocabulary is short.** A model that needed a new word
every third extraction would be a model that had not found its joints.

### `env_set_test.go` contained no tests for `env_set.go`

The twelfth shared package, `internal/yamledit` (three callers), came out with this — and splitting
its tests turned up the sharpest instance yet of the stranded-test pattern.

`env_set_test.go` held six functions. **Every one of them tested something else**: four tested
`yamledit` (`SetSpecPath`, `EditSpecFile`, `IsPerEnvPath`, `ParseAssignments`), one was a package-main
helper, one tested `lineDiff` from `render.go`. Zero tested `env_set.go`.

The `coverage_tier*` files are mis-filed because they are named for a **metric**. This one is
mis-filed because it is named for the **command whose implementation happens to call the code** —
a different cause with an identical effect: a test whose location says nothing about its subject. Ten
tests have now been relocated across this branch, and the two naming patterns account for all of them.

### What `assert-network` corrected — a name that read like a capability

Twenty-second, **below 30,000**, and the best ratio of the lot: **closure 6 across 1,267 lines**.

```
assert-network  assertion:verified "network-enforcement"   [cluster-read]
                assertion:verified "admission-enforcement" [cluster-read]
                assertion:verified "net-probe"             [read-repo]
                assertion:verified "wave-health-vap"       [cluster-read]
```

**I declared `net-probe` wrong first, and the model caught it for a better reason than it gave.** I
wrote it as a `transition` holding `cluster-write`, reasoning that you cannot assert a NetworkPolicy
denies a connection without a pod to attempt it from. `Validate()` refused — *"a transition binding
cannot attach to `verified`"* — and the refusal was correct, but the real error was upstream of it:

**the lane does not create the pod. It is the code that runs inside it.** Its entire body is a
`net.DialTimeout` and an exit code; the pod is created by the workflow, and *"exit codes are the
interface"* is the first thing the file says. So it holds no cluster grant at all.

`ci_net_probe.go` **sounds** like a thing that probes the cluster. It is a thing the cluster runs.
That is the filename-is-not-evidence lesson from the other direction — five catalog corrections have
been "these files do not belong together", and this one was "this file does not do what its name
implies". Reading the code beat reasoning about the name, as it has every time.

**`ExecCombined` is a capability here, not a convenience.** The enforcement lanes assert a connection
is *refused*, which means reading the failure TEXT: a NetworkPolicy drop usually blackholes (timeout)
while an Istio sidecar refusing plaintext resets the connection (refused). Both are "blocked" for the
gate, but which one happened is the difference between an operator checking Cilium and checking
PeerAuthentication. An error-gated, stdout-only read discards exactly that — the same distinction
`internal/kubectlprobe` draws between *absent* and *we never got an answer*.

**One coupling test moved to package `main`** rather than being split: the wave-health VAP's CEL and
the Go guard's allowlist must not drift, and `waveHealthAllowedKinds` is still in
`ci_wave_health_guard.go`. `main` is the side that can see both halves. When `wave-health` is
extracted the test moves with it and the assertion becomes cross-package.

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
   and the action/predicate split in `health.go` on day one, which is where the design either
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
