---
name: docs
description: Auditing, writing or restructuring this repo's Markdown. Use when asked to review docs for accuracy, readability, staleness or structure; when adding a runbook/playbook/ADR/design doc; when adding diagrams or tables of contents; or when a docs change trips docs-guard or chart-version-guard. Encodes what two full audits of all ~108 files found, including the mistakes each audit made.
---

# Working on LLZ documentation

Two full audits have been done. **PR #406** asked *is this true?* **PR #411**
asked *can this be read?* Both found real defects; both also got things wrong in
ways worth not repeating. This file is the residue.

## Before anything else

```bash
cd tools && go run ./cmd/llz ci docs-guard --root ..   # or: make docs-guard
```

It validates every Markdown file against the repo it documents — `llz` flags
against the live cobra tree, `gh workflow run` inputs against declared inputs,
every relative link (here **and** in the post-`deliver-docs` instance), and every
`<!-- toc -->` entry against that file's headings. Start green, end green.

## Rule 0 — measure against `origin/main`, not your working tree

The readability audit was computed against a stale branch. Four of its findings —
a duplicate ADR `0002`, a broken `0011` link, a missing ADR index, a missing
`docs/README.md` — were **already fixed on main**, or were never real. That is a
whole section of a report, wrong, because of one `git checkout` never done.

```bash
git fetch origin && git checkout -b docs/<topic> origin/main
```

Local `main` in this repo is an unrelated-history stub. Always branch from
`origin/main`.

## Rule 1 — the rot is in the DELIVERED set

`llz ci deliver-docs` copies only these into every instance:

```
docs/quickstart.md   docs/runbooks/**   docs/playbooks/**
```

That is the inversion #406 found: **the least-maintained docs are the ones every
adopter carries**, and three of them are opened for the first time during an
incident. The reference docs (`secrets.md`, `adopter-guide.md`,
`landing-zone-spec.md`) are demonstrably maintained. Spend your attention on the
delivered 14, in this order: playbooks → runbooks → quickstart.

`docs/README.md` is **written by `deliver-docs` at render time**. Never create one
in the template — `renderTimeArtifact` in `ci_docs_guard.go` exists because of it.

## Rule 2 — the question to ask each instruction

> **How can an operator get this wrong, and what happens when they do?**

The expensive findings were never subtle prose problems. They were procedures
that *ran clean and did nothing*:

| Class | Example |
|---|---|
| **Silent success** | `llz openbao set` dry-runs and **exits 0** without `--yes`. Two rotation playbooks omitted it: you rotate the password in the product, see success, leave OpenBao stale. |
| **Right shape, wrong target** | An `lokiPushUrl` naming a Service nothing creates, with a NetworkPolicy allowing egress to the same empty namespace. Internally consistent, granting nothing, reviewing clean. |
| **Blast-radius claim that is false** | `pvc-*` presented as a scoping guarantee, after the labeler had started renaming volumes to `<env>-<ns>-<pvc>`. |
| **Doc contradicts itself** | `loki-access.md` said "multi-tenancy is OFF … tenancy is not the cause" while Loki runs `auth_enabled: true` — and contradicted itself five more times in the same file. |

## Rule 3 — verify against the built binary, not `--help`

The `--env` flag on `llz validate` was reported as *removed*. It is **deprecated
and hidden** — it still works and prints a notice. A `--help` sweep cannot see
hidden flags.

```bash
cd tools && go run ./cmd/llz <the exact command from the doc>
```

Run it. docs-guard now reports the deprecated-but-working class explicitly,
because "nothing fails" is precisely why a doc keeps teaching a renamed flag.

## Rule 4 — ADRs and designs are dated records

They describe a decision **at a moment in time**, including things later rejected
or never built. An ADR naming a since-removed flag is doing its job; rewriting it
to match today's CLI *falsifies the record*. `docs-guard` exempts `docs/adr/` and
`docs/designs/` from the command/flag check and still checks their links.

- **ADRs** — [`docs/adr/README.md`](../../../docs/adr/README.md) has the index,
  the reserved numbers (0001, 0011), and why two `0007`s deliberately collide.
  Take the next free number **from the table**, never `ls | tail -1` — that is how
  both collisions happened.
- **Designs** — [`docs/designs/README.md`](../../../docs/designs/README.md) fixes
  a five-value vocabulary: `Shipped` / `Partial` / `Proposed` / `Superseded` /
  `Abandoned`. `Partial` **must** name which phases landed. Prefer the weaker
  claim; classify against the tree (does the verb exist? is the job wired?), do
  not guess. Update the file and the index together.

## Rule 5 — show the graph, don't describe it

If a section describes a flow, a state machine, a topology or a pipeline, it is a
graph and should be drawn. [`docs/architecture/overview.md`](../../../docs/architecture/overview.md)
is the house style: diagram → "key relationships" table → numbered "how to read
this".

- **Graphs become mermaid. Trees stay ASCII.** `forge-abstraction.md`'s two-plane
  listing and `llz-cluster/README.md`'s module tree carry per-node file
  references a box diagram drops. Converting them would be a regression.
- **Put the load-bearing fact in the geometry**, not a caption. "No replication
  between these two clusters" is the reason `llz openbao set` dual-writes; as a
  caption a skimmer misses it, as a red dashed edge they cannot.
- **Validate that it parses.** A broken diagram renders as an error box and
  nobody notices. There is no node toolchain in this repo, so do it out-of-tree:

```bash
npm install mermaid@11 jsdom          # in a scratch dir, NOT the repo
# mermaid needs a DOM; without one every diagram fails identically on
# "DOMPurify.addHook is not a function" — an environment fault, not syntax.
# Set global.window/document/DOMParser/NodeFilter from a JSDOM instance
# (navigator needs Object.defineProperty — it is getter-only), then
# `await mermaid.parse(src)` per block.
```

## Rule 6 — tables of contents, and the anchor trap

`ci_docs_guard.go` refuses "a hand-maintained list that would rot the same way",
and a TOC is exactly that list. It is allowed only because it is **delimited**
(regenerable) and **checked**:

```bash
python3 template-scripts/gen-toc.py --level=2 docs/<file>.md   # inserts or refreshes
```

**The anchor rule is `github-slugger`, and it is not what you would guess.** It
does `.replace(/ /g, '-')` — one hyphen **per space**, not a collapse of runs. So
punctuation between two spaces leaves **two** hyphens:

```
"Writing / rotating secrets — dual-write"  ->  writing--rotating-secrets--dual-write
```

It also **keeps** the U+FE0F variation selector, so `⚠️ Heading` slugs with a
leading invisible character.

I got this wrong, and the guard agreed with me — because the generator and the
checker shared the rule, so a wrong anchor was compared against a wrong anchor.
That is why `tools/cmd/llz/testdata/github_slugs.json` is an **oracle**: every
heading in the repo paired with the slug the real implementation produced.
**Only an oracle catches a shared-assumption bug.** Regenerate it with
`github-slugger` if headings change substantially.

Do not add a TOC to every long doc. GitHub renders an outline button; a TOC
nobody regenerates is worse than none. The ~15 that have one are the ones read
under time pressure.

## Rule 7 — measure prose as prose

The readability audit reported "nine paragraphs over 250 words, the worst 1,574"
and recommended splitting them. **All but a handful were bullet lists**, which are
already structured. `lessons-learned.md` is 2,708 list words against 151 prose
words — the proposed "restructure it as a table" would have destroyed detail that
does not fit a cell.

When measuring block density, exclude any block whose lines start with
`- `, `* `, `1. `, `|`, `#` or `>`. Real count across `docs/`: **6** prose blocks
over 150 words, largest 245. There is no paragraph-bloat problem here.

## Rule 8 — the root README indexes what is LIVE, not what happened

Index real estate is the scarcest in the repo, and it is the one place a reader
assumes everything listed is current. Point-in-time records do not belong there
even when they are excellent:

- **Version-pair upgrade designs** (`apl-core-v6-migration`, `apl-core-v61-upgrade`)
  are records of a move that already happened. They live in
  [`docs/designs/README.md`](../../../docs/designs/README.md) with a status.
- **ADRs and designs generally** — one row pointing at each index, never rows per
  document. The indexes exist so the README does not have to grow.

And **do not enumerate a directory in the README.** That list is hand-maintained
and it rots: this repo's had already lost 4 of 11 runbooks and `first-workload`,
the playbook a new adopter needs most. Give the directory its own index —
[`runbooks/README.md`](../../../docs/runbooks/README.md) is by **symptom**,
[`playbooks/README.md`](../../../docs/playbooks/README.md) by **task** — which
puts the list next to the files it describes, where an author changing them will
see it. Both directories are in the `deliver-docs` keep-set, so those indexes
ship to adopters too.

Rule of thumb: if a row would still be there after the thing it describes is
finished, it belongs in a sub-index, not the README.

## Rule 9 — historical content: which kind, and where

Not all history is rot. A sweep of all ~111 files found the split is clean, and
it is about **whether the reader can resolve the reference**:

**Keep — "where did X go?" notes.** `secrets.md`'s *"`secret/harbor/admin` is NO
LONGER seeded here — ESO writes it in-cluster via PushSecrets"*, or
`linode-credential-rotation.md`'s *"`CLOUD_FIREWALL_TOKEN` was retired"*. An
operator upgrading an older instance searches for the thing that vanished. These
are load-bearing and should not be trimmed.

**Delete — author-seat references.** **`this branch` / `this PR` in a live doc is
always wrong the moment it merges.** `alerting.md` carried four, describing chart
fixes that shipped many versions ago (`llz-openbao-platform` 0.1.18 → now 0.1.30)
against an issue that is closed. A reader today cannot resolve "this branch" to
anything.

Rewrite as a **standing fact plus the version it landed in** — *"since chart
0.1.18, the NetworkPolicy grants …"* — which stays true and still tells an
operator on an older chart what they are missing.

That rewrite also found a **real open gap the stale framing was hiding**: the doc
said Harbor's alerts were "provisional pending a spot-check", when in fact the
alerts shipped and it is the *scrape gate* that never landed — Harbor's
ServiceMonitor is still absent from `defaultScrapeMonitors`, so nothing fails if
its metrics stop. Stale status text is not just noise; it conceals current state.

Two measurement traps when sweeping for this:

- `#\d{2,4}` matches **mermaid hex colours** (`fill:#4285f4` → `#4285`). It made
  `architecture/overview.md` look like it had 8 issue references; it has none.
- Most `the old …` hits in rotation runbooks are **procedural**, not historical
  ("revoke the old token *after* the new one authenticates"). Read them.

Archival files — `docs/adr/`, `docs/designs/`, `docs/workflows/`,
`lessons-learned.md` — are exempt. History is their content.

## Rule 10 — code comments: the same rule, one narrow exception

The Markdown rules above were applied to the Go/YAML comment tree. **Measured
first, and the result was: do not mass-trim.**

| | |
|---|---|
| Comment ratio, non-test Go | 28% (20,275 / 73,088 lines) |
| Comment ratio, tests | 13% |
| Identical long comment lines across a source/`_test.go` pair | **8**, nearly all section banners |
| Author-seat (`this branch` / `this PR`, git sense) | **6** — all fixed |

The historical narration reads as over-commenting by volume and is not. Comments
like *"an earlier cut terminated on `<` to catch input redirects, so the scan
stopped at the placeholder and skipped every flag after it on ~92 invocations"*
each record a specific failure, its silent consequence, and why the current rule
is shaped as it is. That is the knowledge that stops the next person re-making
the same cut. **Deleting it is a regression, not a cleanup.**

The one class that IS wrong is the same as Rule 9's: `this branch` / `this PR`
meaning *the change I am authoring*. Once merged, nobody can resolve it.

**But most `this branch` hits in Go are the CONTROL-FLOW sense** — "reaching this
branch means the API refused" — which is correct and must be left alone. Of 11
matches, only 3 in Go and 3 in YAML were the git sense. Read every one; a blind
sweep would have mangled the other five. (`lint.yml`'s "the CLI IN THIS PR" is
also correct — that workflow genuinely runs on a PR.)

Rewrite the git-sense ones as a standing fact plus the version that carries it,
exactly as in Rule 9 — and keep the prose comment and its Markdown counterpart
consistent, since the alert YAML and `alerting.md` describe the same thing and
had drifted into the same stale story.

## Rule 11 — de-duplicate, but do not merge different questions

Real duplication: the `secrets: inherit` / `required: false` rationale was
written at full length in **both** workflow docs — the second even said "same
rationale as in `llz-terraform.yml`" and then restated it anyway. Make one
canonical, link the other, keep only what is genuinely local.

Not duplication: three apl-core documents that look interchangeable in an index
are a cutover **runbook** plus two version-pair **designs**. The audit proposed
merging them; that was wrong. Label them by the question each answers instead.

## Rule 12 — a chart README edit needs a Chart.yaml bump

`chart-version-guard` fires on any edit inside a chart directory, because
`publish-charts.yml` only pushes a **new** version. A README-only change still
needs the bump, and it **cascades**: `llz-argo-bootstrap-apps` pins the other
charts, an unmoved pin 404s at Argo sync time, so moving the pin bumps that chart
too. Do not weaken the gate in the PR that trips it.

## Before you open the PR

```bash
cd tools && go run ./cmd/llz ci docs-guard --root .. && go test ./... && go vet ./...
make instance-test          # if you touched instance-template/ or deliver-docs
```

- Check `git status` for files you did not mean to commit. This branch swept up
  an untracked ADR belonging to another effort and needed a rebase to remove it.
- **Publish corrections to your own audit.** #406 lowered the severity of its own
  deprecated-flag finding in the PR body; #411 retracted four findings and two
  Tier-3 recommendations. A report nobody can trust to correct itself is worth
  less than a shorter honest one.

> Note: this file deliberately avoids writing a deprecated flag as a runnable
> invocation, because docs-guard would (correctly) report it. Rephrasing is the
> fix; an ignore-list would be "a place to bury real breakage".
