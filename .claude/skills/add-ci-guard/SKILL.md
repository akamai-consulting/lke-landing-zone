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
   `llz ci <verb>` subcommand in its own guard package under
   `tools/internal/extensions/guards/<name>/`, registered in
   `tools/internal/cli/ci.go`. The package declares itself in `extension.go` with
   a **gate** binding — `read-repo` and nothing else, which is what makes the gate
   kind legal. (It does **not** go in `tools/cmd/llz`: that package is a six-line
   entry point budgeted by `cmd-llz-entrypoint`.) The
   `untestable-loc-check` gate exists precisely to force this — inline
   workflow/Makefile shell logic is budgeted by `.untestable-budget.yaml` and
   budgets only ratchet DOWN. Read `tools/AGENTS.md` first: stdlib-first
   (cobra + `sigs.k8s.io/yaml` only), static builds.

2. **Unit-test the decision logic.** `make coverage` enforces per-package floors
   (`COVERAGE_MINS` in the Makefile). The floors are a ratchet — your new code
   must not drop the package below its floor. Test the pure decision function,
   not the cobra glue.

3. **Join the CI set with a REGISTRY edit, not a Makefile edit.** Add a row to
   `Gates()` in `tools/internal/shared/extension/registry/gates.go` and the
   command to `tools/internal/cli/ci.go`. That is the whole of it: `llz ci gates`
   drives whatever the declarations say, and `make llz-gates` is how CI calls it.

   > Thirteen per-guard targets were collapsed into `llz-gates` precisely because
   > the Makefile and the registry each held a list of which guards exist, and the
   > registry's drifted. Do not add your guard to `LINT_K8S` or `LINT_TF` — it is
   > already in the CI set the moment the registry row lands. The Makefile block
   > above `llz-gates` explains this; read it before adding a target.

   Two fields exist for the unusual cases: `Flag`/`Subtree` when the gate takes
   its tree on something other than `--root`, and `NewWithTree` when it needs the
   live cobra tree (only `docs-guard` does — run through plain `New()` its command
   is parentless and its largest check silently passes over nothing).

4. **Add a target only for `--only` iteration**, and only carrying Makefile
   knowledge the driver cannot hold:

   ```make
   my-new-guard: export LLZ_FORCE_SOURCE := 1
   my-new-guard:
   	$(call LLZ_CI,gates --only my-new-guard,)
   ```

   Set `LLZ_FORCE_SOURCE := 1` when the guard compares the working tree against
   ITSELF — the prebuilt image binary is built from the merge-base and will not
   even have your verb on the PR that introduces it (`managed-lock-check`,
   `version-pins-check`, `docs-guard`, `source-ref-guard` are the models). Add a
   `: render-charts` prerequisite if it reads rendered chart output, and hard-fail
   on a missing rendered tree rather than passing green over a corpus it never saw.

   **Do not hand-roll `if command -v llz`.** The `LLZ_CI` macro detects an
   uncommitted `tools/` tree and builds from source, because otherwise the
   installed binary answers for code you did not write.

   Add a comment block above the target explaining the FAILURE MODE it prevents
   and the PR/issue where it bit (the "scars as defaults" convention in
   `AGENTS.md`). A guard that needs a git base ref to diff against (like
   `chart-version-guard`) stays out of the driver and gets its own workflow — the
   CI lint containers have no base ref.

5. **Declare it**: add a `.PHONY` entry, a `help:` line, and — if it gets its
   own workflow — follow `.github/workflows/AGENTS.md` (SHA-pinned `uses:`,
   explicit `permissions:` block per job, GitHub-hosted runners).

6. **Wire it into the change-aware `lint` recipe** so it runs on the diffs it
   cares about. This is the step the registry does NOT do for you, and skipping it
   is the likeliest way to ship a gate nobody runs: `llz-gates` is reached from
   that recipe only on a `kubernetes-charts/` change, so a guard whose corpus is
   Markdown or Go needs its own `grep -qE` line invoking the `--only` target.

   `docs-guard` shipped exactly that way — tested and wired into `make lint`,
   while the `paths:` filter matched no Markdown, so the one change class it was
   built for could not run it.

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
- [ ] Row in `registry/gates.go` + command in `internal/cli/ci.go` (or its own
      workflow, with rationale) — NOT a `LINT_K8S` / `LINT_TF` entry
- [ ] Reachable from the change-aware `lint` recipe for the paths it guards
- [ ] `.PHONY` + `help:` entries
- [ ] `make lint` and `make coverage` exit 0
- [ ] No org-identity hardcoding — the guard must work for any adopter fork
