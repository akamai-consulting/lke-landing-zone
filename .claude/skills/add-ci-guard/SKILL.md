---
name: add-ci-guard
description: Scaffold a new wedge-class CI guard following this repo's established pattern - unit-tested Go decision logic as an `llz ci` verb, a thin Makefile glue target with the command-v fallback idiom, membership in a lint group, and a scars-as-defaults comment. Use when a production failure class needs a permanent PR-time gate, when asked to "add a guard", "add a CI check", or "gate this failure class".
---

# Add a wedge-class CI guard

This repo converts every production wedge into a permanent, unit-tested PR-time
gate. The pattern is identical across all existing guards — follow it exactly.
Study one existing guard end-to-end before writing anything; the best references
are `wave-health-guard` (PR #142), `wave-dependency-guard` (#163),
`mesh-egress-guard`, `monitoring-label-guard` (#175), `chart-pin-guard`, and
`chart-version-guard` — all visible in the `Makefile` with comment blocks
explaining the failure mode each one prevents.

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

3. **Makefile target is thin glue** using the repo's fallback idiom:

   ```make
   my-new-guard:
   	@if command -v llz >/dev/null 2>&1; then \
   		llz ci my-new-guard; \
   	else \
   		cd $(GO_DIR) && go run ./cmd/llz ci my-new-guard --root ..; \
   	fi
   ```

   Add a comment block above it explaining the FAILURE MODE it prevents and the
   PR/issue where it bit (the "scars as defaults" convention in `AGENTS.md`).
   If it inspects rendered chart output, depend on `render-charts`.

4. **Wire it into the right group** in the Makefile: `LINT_K8S` (runs in the CI
   `kubernetes` container job) or `LINT_TF` (the `terraform` container job).
   Guards needing a git base ref to diff against (like `chart-version-guard`)
   stay OUT of these groups and get their own workflow — the CI lint containers
   have no base ref.

5. **Declare it**: add a `.PHONY` entry, a `help:` line, and — if it gets its
   own workflow — follow `.github/workflows/AGENTS.md` (SHA-pinned `uses:`,
   explicit `permissions:` block per job, GitHub-hosted runners).

6. **Verify** with `make <my-new-guard>` locally, then `make lint` (the
   authoritative gate — must exit 0), and `make coverage`.

## Checklist before opening the PR

- [ ] Go verb + unit tests in `tools/` (`gofmt -w`, `go vet`, `go test ./...` clean)
- [ ] Makefile target with `command -v llz` fallback + failure-mode comment
- [ ] Added to `LINT_K8S` / `LINT_TF` (or its own workflow, with rationale)
- [ ] `.PHONY` + `help:` entries
- [ ] `make lint` and `make coverage` exit 0
- [ ] No org-identity hardcoding — the guard must work for any adopter fork
