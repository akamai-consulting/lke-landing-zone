---
name: add-ci-guard
description: Scaffold a new wedge-class CI guard following this repo's established pattern - unit-tested Go decision logic as an `llz ci` verb, a thin Makefile glue target using the LLZ_CI macro, membership in a lint group, and a scars-as-defaults comment. Use when a production failure class needs a permanent PR-time gate, when asked to "add a guard", "add a CI check", or "gate this failure class".
---

# Add a wedge-class CI guard

This repo converts every production wedge into a permanent, unit-tested PR-time
gate. The pattern is identical across all existing guards — follow it exactly.
Study one existing guard end-to-end before writing anything; the best references
are `wave-health-guard` (PR #142), `wave-dependency-guard` (#163),
`mesh-egress-guard`, `monitoring-label-guard` (#175), `chart-pin-guard`, and
`chart-version-guard` — all visible in the `Makefile` with comment blocks
explaining the failure mode each one prevents.

> **This is the mechanics of a STATIC guard.** Whether a static guard is the
> right layer at all — versus a coupling test or a live `assert-*` lane — is the
> `gate` skill's question, and it is worth answering first. A static guard cannot
> see two values that are consistent with each other and both wrong.

## The pattern, step by step

1. **Decision logic lives in Go, never in bash.** Implement the check as a new
   `llz ci <verb>` subcommand in `tools/cmd/llz/` (the `ci` subtree). The
   `untestable-loc-check` gate exists precisely to force this — inline
   workflow/Makefile shell logic is budgeted by `.untestable-budget.yaml` and
   budgets only ratchet DOWN. Read `tools/AGENTS.md` first: stdlib-first
   (cobra + `sigs.k8s.io/yaml` only), static builds.

2. **Unit-test the decision logic.** `make coverage` enforces per-package floors
   (`COVERAGE_MINS` in the Makefile). The floors are a ratchet — your new code
   must not drop the package below its floor. Test the pure decision function,
   not the cobra glue.

3. **Makefile target is thin glue** using the `LLZ_CI` macro:

   ```make
   my-new-guard:
   	$(call LLZ_CI,my-new-guard,--root ..)
   ```

   `$(1)` is the verb plus any args spelled from the REPO ROOT; `$(2)` is the
   same args re-based for the from-source branch, which runs one level down in
   `tools/`. Both get passed and the last `--root` wins — that apparent duplicate
   is load-bearing, so don't "simplify" it away.

   **Do not hand-roll `if command -v llz`.** The macro carries logic a bare
   fallback does not: it detects an uncommitted `tools/` tree and builds from
   source, because otherwise the installed binary answers for code you did not
   write. Set `LLZ_FORCE_SOURCE := 1` on the target when the guard compares the
   working tree against ITSELF — the prebuilt image binary is built from the
   merge-base and will not even have your verb on the PR that introduces it
   (`managed-lock-check` and `version-pins-check` are the models).

   Add a comment block above it explaining the FAILURE MODE it prevents and the
   PR/issue where it bit (the "scars as defaults" convention in `AGENTS.md`).
   If it inspects rendered chart output, depend on `render-charts` — and make the
   guard hard-fail on a missing rendered tree rather than passing green over a
   corpus it never saw.

4. **Wire it into the right group** in the Makefile: `LINT_K8S` (runs in the CI
   `kubernetes` container job) or `LINT_TF` (the `terraform` container job).
   Guards needing a git base ref to diff against (like `chart-version-guard`)
   stay OUT of these groups and get their own workflow — the CI lint containers
   have no base ref.

5. **Declare it**: add a `.PHONY` entry, a `help:` line, and — if it gets its
   own workflow — follow `.github/workflows/AGENTS.md` (SHA-pinned `uses:`,
   explicit `permissions:` block per job, GitHub-hosted runners).

6. **Wire it into the change-aware `lint` recipe** so it runs on the diffs it
   cares about. A guard that only exists in a group nothing triggers is a gate
   nothing invokes — `docs-guard` shipped that way, tested and wired into
   `make lint`, while the `paths:` filter matched no Markdown, so the one change
   class it was built for could not run it.

7. **Verify** with `make <my-new-guard>` locally, then `make lint` (the
   authoritative gate — must exit 0), and `make coverage`.

## Fail closed

A guard that reports success having examined nothing is worse than no guard: it
launders an absence of evidence into a green check, and "examined nothing" is
what a broken corpus looks like. An empty scan, an unparseable input, or a
missing rendered tree are FAILURES, not passes — and never derive the expected
set from the thing under test, or a regression at the source empties the set and
the guard goes green on the very bug it exists to catch. The `gate` skill has the
full doctrine.

## Checklist before opening the PR

- [ ] Go verb + unit tests in `tools/` (`gofmt -w`, `go vet`, `go test ./...` clean)
- [ ] Makefile target using `$(call LLZ_CI,…)` + failure-mode comment
- [ ] Fail-closed arms tested: empty corpus, malformed input, unreadable path
- [ ] Added to `LINT_K8S` / `LINT_TF` (or its own workflow, with rationale)
- [ ] Reachable from the change-aware `lint` recipe for the paths it guards
- [ ] `.PHONY` + `help:` entries
- [ ] `make lint` and `make coverage` exit 0
- [ ] No org-identity hardcoding — the guard must work for any adopter fork
