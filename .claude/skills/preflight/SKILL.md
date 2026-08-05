---
name: preflight
description: Passing this repo's gates before pushing, and reading each failure correctly. Use before opening or updating a PR, when `make lint` goes red, when a budget ratchet (untestable-loc, coverage, docs-guard) trips, or when CI is red on a branch that is green locally. Encodes which gates run on which change and where local and CI diverge.
---

# Getting through the gates

`make lint` is the authoritative final gate — [`AGENTS.md`](../../../AGENTS.md)
"Before submitting" is canonical. This file is what that page cannot tell you:
**which** gates your diff triggers, where local and CI disagree, and what each
red one actually means.

## The order that saves time

```bash
cd tools && gofmt -w . && go vet ./... && go test ./...   # anything Go
make lint                                                  # the real gate
```

`make lint` is **change-aware** — it keys off `git diff HEAD` and runs a different
subset per diff. Two consequences worth internalising:

- **With nothing uncommitted it runs nothing** and prints so. A green `make lint`
  immediately after committing has checked *your working tree*, not your commit.
  Use `make LINT_ALL=1 lint` when you need the unconditional sweep.
- **It keys off paths, so moving a file between trees changes which gates run.**
  Terraform gates cover both `terraform-modules/**` and the instance roots; Helm
  gates key on `kubernetes-charts/`.

## Where local and CI disagree

This is the top source of "green locally, red in CI".

- **`lint.yml` runs from the MERGE ref.** Main's newer steps gate your branch even
  when your branch's own copy of the workflow lacks them. Rebase on `origin/main`
  before concluding a CI failure is unrelated to you.
- **`lint.yml` only triggers on PRs targeting `main`/`master`.** A stacked PR based
  on a feature branch runs **no lint at all**. See the `branch-base` skill.
- **`lint.yml` has a `paths:` filter.** A diff touching none of those paths runs
  none of it. Do not read the absence of a red check as a pass.
- **`k8s-lint` runs on the RENDERED output**, not the raw component tree. A
  manifest that looks fine in place can fail after `make render-charts`.
- **`chart-version-guard` needs the repo root as cwd.**
- **CI has no `tofu` in some jobs**, so anything emitting HCL must emit it
  pre-formatted — a formatter that no-ops locally is not a formatter in CI.

## Reading the budget ratchets

Three gates are **ratchets**: they encode a number that may only go down. All
three fail with a message telling you the remedy; the remedy is never "raise the
number", and the files say so in their own comments.

### `untestable-loc` — logic that cannot be unit-tested

`.untestable-budget.yaml` caps six categories — inline workflow bash, shell
scripts, Python scripts, Terraform provisioner heredocs, makefile recipes, and
shell embedded in YAML block scalars. **Read the live budgets from the file, not
from here**: they are a ratchet, and a number restated in a second place goes
stale in the direction that reads as headroom you do not have.

```bash
cd tools && go run ./cmd/llz ci untestable-loc --verbose   # per-file breakdown
```

The fix is to move the logic into `tools/cmd/llz` as a tested verb. Genuine
install/glue with no logic worth testing goes in `exclude:` **with a one-line
justification** — that is the sanctioned escape, not a budget bump.

It has been raised exactly once, to unblock six new e2e gates, and the debt was
paid inside the same change so the net was still a ratchet down. That is the bar.

> **Do not write a new helper as a shell or Python script.** The first cut of one
> recent helper was a 74-line Python script and failed this gate on its first CI
> run; the gate's own message says the fix is to move it into `tools/cmd/llz`.
> Note the output is `used / budget` — a category reading `0 / 60` has **no
> Python in the tree**, which is the state to preserve, not 60 lines of room to
> spend.

### `check-coverage` — per-package floors

`COVERAGE_MINS` in the [`Makefile`](../../../Makefile) holds a floor per package.
Bump a floor **up** as coverage improves, never down — so read the current floors
there rather than from any copy.

```bash
make coverage
```

A floor that is red because of *pre-existing* debt in a package you merely touched
is worth saying so in the PR body rather than silently lowering.

### `docs-guard` — doc drift

```bash
make docs-guard
```

It validates **every** Markdown file in the repo (including `.claude/skills/`)
against the tree: `llz` flags against the live cobra tree, `gh workflow run`
inputs against declared inputs, and every relative link — resolved both here and
in the post-`deliver-docs` instance. See the `docs` skill before fixing findings.

## What each guard is actually asserting

`make lint` composes dozens of named targets. When one goes red, `make help` gives the
one-line summary and the target's own comment block in the
[`Makefile`](../../../Makefile) gives the failure mode it was built against —
those comments are the documentation, and they are usually specific about which
outage the guard came from. Read the comment before working around the guard.

The security-shaped ones travel together and are covered by the
`credential-change` skill: `credential-coverage-guard`, `at-rest-guard`,
`plaintext-guard`, `mtls-wiring-guard`.

## Two gates that are deliberately NOT gates

Do not "fix" these into the build:

- **`make deadcode`** is report-only. "Unreachable from main" and "should be
  deleted" are different claims, and this repo has three legitimate reasons for
  the gap — gating would force the benign classes to be suppressed forever, which
  trains people to ignore the real one.
- **`make fuzz`** is on-demand. Fuzzing is non-deterministic; gating on it makes
  the build flaky rather than safe. The **seed corpora** run as ordinary subtests
  on every `go test`, which is the deterministic half. A crasher the toolchain
  writes to `testdata/fuzz/` **must be committed** — that is how a one-off find
  becomes a permanent test.

## Before you open the PR

1. `make lint` exits 0.
2. **Name the gate** that would catch your change regressing — one line in the PR
   body. If the honest answer is "none", write one (see the `gate` skill). If the
   change genuinely does not need one, say which and why. **A green `make lint` is
   not evidence a behavior works** — both regressions this rule comes from were
   green.
3. `git status` for files you did not mean to commit. This repo has untracked
   files belonging to other efforts sitting in the working tree; a `git add -A`
   sweeps them in and costs a rebase.
4. No `Co-Authored-By` trailer, no agent identity as author or committer.
