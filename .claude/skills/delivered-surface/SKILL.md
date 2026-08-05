---
name: delivered-surface
description: Changing anything an adopter's instance actually carries — instance-template/, the vendored workflows, or the delivered docs. Use when editing instance-template, when template-manifest-check / managed-lock-check / the upgrade-churn guard trips, or when an upgrade diff comes out larger than the change. What ships to instances is governed by different rules than the rest of the repo.
---

# Changing what an instance carries

This repo ships **reusable artifacts, not a running deployment**. Two properties
of the delivered surface are load-bearing and neither is obvious from the code:

1. **The delivered scaffold contains no version at all.**
2. **The least-maintained docs are the ones every adopter carries.**

[`AGENTS.md`](../../../AGENTS.md) "Publishing & versioning discipline" is
canonical.

## What is actually delivered

| Surface | Where |
|---|---|
| The scaffold | `instance-template/` — renders to the **instance root** |
| Vendored workflows + composite actions | `instance-template/.github/`, covered by `.template-managed.lock` |
| Docs | `quickstart.md`, `runbooks/`, `playbooks/` — everything else is pruned |

Everything under `terraform-modules/`, `kubernetes-charts/` and `tools/` must stay
**org-agnostic**. Instance-specific literals live only in `instance-template/`.
No org or `platform-` prefix on TF resource names, Helm resource names or release
names — generic names are what let two system teams share a cluster.

## Rule 1 — never render a version into the delivered surface

The pin lives **once**, in `.copier-answers.yml`, and everything reads it at
runtime. The one deliberate exception is the pinned docs pointer, which
`llz ci deliver-docs` **generates** at render time rather than storing.

This is not a style preference. A version restated in the scaffold adds that many
lines to **every instance's upgrade diff, on every release, forever**. One real
instance spent 45 of 53 changed lines on a content-free version bump — 27
version-pinned doc permalinks, 10 workflow inputs, and prose — all restating the
one fact copier already records. Nothing in CI objected, because each addition
looked reasonable on its own.

**Restating the pin in prose counts.** Link to `.copier-answers.yml` instead.

The guard runs on **both sides** of the delivery: in the template it stops the
version being re-rendered; in an instance checkout it catches strings an older
`llz` actually delivered — which happened, and the instance's own CI did not
object either.

## Rule 2 — a link that resolves here can be dead there

A file under `instance-template/` renders to the **instance root**, so its links
must be judged as they will appear there. **A rendered instance has nothing above
its root**, so any target that climbs past it is dead however well it resolves in
the template. That is how a `../../platform-apl/` link passed for months while
being dead in every instance.

For the delivered docs, `deliver-docs` rewrites links that point at pruned files —
but only where the rewrite can **see** them, which is why root-level files needed
their own pass. `docs-guard` now evaluates the keep-set against the keep-set.

**Never create `docs/README.md` in the template** — `deliver-docs` writes it at
render time.

## Rule 3 — the delivered docs are the ones read during an incident

An audit of every Markdown file found the rot concentrated in the delivered
operator set, several written against an older platform and never revisited. The
expensive findings were procedures that **ran clean and did nothing**: a rotation
step that dry-runs and exits 0 without `--yes`, a break-glass recipe pointed at a
listener that could only ever refuse it, a sweep whose blast-radius claim stopped
being true when a labeler started renaming its targets.

Use the `docs` skill for this work. Ask one question per instruction: **how can an
operator get this wrong, and what happens when they do?**

## Rule 4 — the locks move with the files

- **`.template-manifest`** — the class table behind template ownership. `make
  template-manifest-check`.
- **`.template-managed.lock`** — covers the template-owned `.github/` files.
  Editing a `llz-*.yml` body without re-running the refresh ships a lock **every
  instance fails on**. `make managed-lock-check`.

Both gates run **from source**, deliberately: they compare the working tree
against itself, and the prebuilt image binary is built from the merge-base — so it
does not even have the verb on the PR that introduces it.

## Rule 5 — gates key on path

Moving a file between trees silently changes which gates run. `lint.yml`'s
`paths:` filter names `instance-template/.github/**` and the two lock files
explicitly; a change elsewhere under `instance-template/` may run nothing. Check
before assuming a green PR was checked.

## Verifying

```bash
make template-manifest-check
make managed-lock-check
make docs-guard
make scaffold-check      # `llz env add` into a throwaway env
make instance-test       # real `copier copy` render + offline validation
```

`instance-test` is the fast local counterpart to release-e2e: it renders through
the **real** instantiation path, which is what catches the `<@ token @>`
substitution bugs a raw copy silently passes.

## The invariant worth restating

A content-free version bump must touch **exactly one file** in a delivered
instance. If your change makes that number larger, it is a churn regression — fix
the change, not the guard.
