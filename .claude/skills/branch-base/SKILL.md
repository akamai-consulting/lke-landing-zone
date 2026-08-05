---
name: branch-base
description: Choosing what to branch from, and rebasing safely. Use before starting any branch, when stacking a PR on another branch, when CI is red for reasons not in the diff, and before rebasing a branch that an e2e run or a live instance is pinned to. Short skill, disproportionate cost when skipped.
---

# Where to branch from, and when not to rebase

Four facts. Each has cost real time in this repo.

## 1. Branch from `origin/main`

```bash
git fetch origin && git switch -c <type>/<topic> origin/main
```

- **Local `main` is an unrelated-history stub.** Branching from it produces a
  branch that cannot merge cleanly and diffs against the wrong tree.
- **Base on the canonical upstream, not a fork.** A fork's default branch is often
  many commits stale even when the content is already merged upstream under
  different SHAs. Terraform module `git::` sources, branch pushes and PRs must all
  target the canonical repo. Verify with `git log origin/main` before starting,
  and check `gh` is wired to the canonical repo.

Measuring against a stale base is not a theoretical risk: one audit reported four
findings — a duplicate ADR number, a broken link, two missing index files — that
were **already fixed on main**. A whole section of a report, wrong, from one fetch
never done.

## 2. A stacked PR runs no lint

`lint.yml` triggers on `pull_request` **only for branches targeting `main` or
`master`**. A PR based on a feature branch runs **none of it** — no Go tests, no
chart gates, no `docs-guard`, no budget ratchets. The stack looks green and has
been checked by nothing.

If you stack (and stacking is the normal workflow here), then before merging the
base:

```bash
make LINT_ALL=1 lint
cd tools && go test ./...
```

…or retarget the PR at `main` once its parent lands and let CI actually run.

## 3. CI runs from the MERGE ref

`lint.yml` executes **main's** copy of the workflow, not your branch's. So main's
newer steps gate your branch even when your branch predates them, and a step your
branch adds does **not** run on your branch's own PR.

Practical consequences:

- A red check naming a step your branch does not contain is real. Rebase on
  `origin/main` before concluding it is unrelated.
- Diff-scoped linters compare against the merge base, so a rebase changes what
  they look at.
- A gate that must compare the working tree against itself has to run **from
  source** — the prebuilt image binary is built from the merge-base and lacks the
  verb on the PR that introduces it. Several `Makefile` targets set
  `LLZ_FORCE_SOURCE := 1` for exactly this reason; do not "simplify" them.

## 4. Do not rebase what something is pinned to

**A running e2e instance pins the SHA it was instantiated at.** Rebasing orphans
that SHA, so the teardown fails — which leaks billable Linode resources and needs
a manual sweep.

Likewise: **never force-push mid-e2e.**

Before rebasing, ask what holds a reference to the current SHA — an in-flight e2e
run, a dispatched workflow, a debug branch in the instance repo. Wait for it, or
tear it down first.

## Before you commit

Check `git status` for files that are not yours. This working tree routinely
carries untracked files belonging to other efforts; a `git add -A` sweeps them in
and costs a rebase to remove. Add paths explicitly.

And per [`AGENTS.md`](../../../AGENTS.md): **no `Co-Authored-By` trailer and no
agent identity** as git author or committer. Commits carry the human contributor
only.
