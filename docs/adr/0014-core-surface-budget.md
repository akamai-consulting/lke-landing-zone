# ADR 0014 — The core-surface budget: cap the destination, not just the source

Status: **accepted**, implemented (`llz ci core-surface`, `.core-surface-budget.yaml`).
Date: 2026-08-03
Relates: [ADR 0013](0013-llz-as-apl-cli.md) (the inward decomposition this gate
pushes toward); issue #10 / PR #15 (the extension framework);
[docs/designs/internal-extension-model.md](../designs/internal-extension-model.md)
(the declaration model this change lands as Phase 1);
[docs/designs/internal-extensions.md](../designs/internal-extensions.md) (the
measured decomposition map — where the budget is going); `.untestable-budget.yaml`
and `llz ci untestable-loc` (the gate this one counterweights).

**This document owns the DECISION** — why package `main` needed a budget,
why the number carries no slack, and the one-ownership-authority corollary.
The measurements it rests on live in [the
catalog](../designs/internal-extensions.md); the extension model is
specified in [the model doc](../designs/internal-extension-model.md). Cited,
not restated.

## Context

This repo has a design principle it actually enforces: decision-making logic
belongs in unit-tested Go, not in CI shell. `llz ci untestable-loc` tallies logic
lines of inline workflow bash, shell scripts, Python, Terraform provisioner
heredocs, Makefile recipes and shell-in-YAML against `.untestable-budget.yaml`,
and fails the build when a category goes over. The budgets ratchet DOWN. The file
records the discipline in unusual detail, including the one time a budget was
raised to unblock a red build and the debt that was then paid to undo it.

**It worked.** Category after category came down as bash became `llz ci` verbs.
The e2e assert battery, `check-coverage`, `chart-lock-drift`,
`argocd-rendered-apps`, `placeholder-lint` — each conversion deleted untestable
shell and replaced it with Go that has tests.

**And that is the problem.** The gate names a *destination* and no *capacity*.
Every conversion lands in one place — `tools/cmd/llz`, package `main` — and
nothing has ever objected to what arrives there. Today, on `main`:

| | at PR #15's merge-base (`670b935b`, 2026-06-28) | today (2026-08-03) |
|---|---|---|
| non-test `.go` files in `tools/cmd/llz` | 93 | **213** |
| of which `ci_*.go` | 49 (53%) | **119 (56%)** |
| Go logic lines in package `main` | — | **41,658** |

The share barely moved; the absolute size more than doubled in five weeks. And
the incentive runs the wrong way: converting 40 lines of workflow bash into a
400-line Go file scores as a **reduction** under the only gate that was watching.
That is not a hypothetical — it is the shape of nearly every conversion above,
and each one was individually correct.

Package `main` is the worst possible destination for that growth. It cannot be
imported, so nothing outside it can be tested against it; it has no boundary, so
any file may reach any other; and it is where `func main` lives, so it will never
be deleted. Two answers to that already exist in this repo, and neither has any
pressure behind it:

- **Inward** — [ADR 0013](0013-llz-as-apl-cli.md), "one binary, two altitudes":
  move code into `tools/internal/<pkg>` behind real package boundaries. There are
  21 such packages today. Every move was voluntary.
- **Outward** — issue #10 / PR #15, the recipe/extension framework: capabilities
  that only some instances want stop being compiled into the core at all.

Both are strategies without a forcing function. A reviewer can ask for
decomposition, but nothing fails if they don't, and 119 `ci_*.go` files say how
that goes.

**They also turned out not to be two strategies.** A full catalog of package
`main` — [docs/designs/internal-extensions.md](../designs/internal-extensions.md),
every one of the 214 files assigned exactly once, summing to the 41,709 this gate
counted when the catalog was taken (re-derived on merge: 37,945 + 3,764 over
195 + 19 files) — found that only **21 of 57** candidate extensions could ever be
external. The other 36 need in-process Go: spec types, credential handles, cluster
clients. So "inward" and "outward" are the same move at different maturities, and
the thing that has to exist first is an **internal** extension model with a Go
action ABI. That is a correction to the framing above, not a refinement of it.

## Decision

**Budget the destination too.** `.core-surface-budget.yaml` caps Go logic lines
in package `main`, enforced by `llz ci core-surface` from `lint.yml` and `make
lint`, on the same ratchet doctrine as its sibling: lower it as code moves out,
never raise it to make a red build green.

**The number is a high-water mark with no slack: 41,803**, the exact measurement
when this was written (215 non-test files, 121 of them `ci_*.go`).

> **Re-baselined on rebase, 2026-08-05: 47,182** across 236 files, 130 of them
> `ci_*.go`. Five weeks of `main` with no decomposition in between — +5,379 lines
> and +21 files. The mechanism the rest of this section argues for is why that
> number had to be restated by hand instead of drifting: the gate went red on the
> rebase and stayed red until the line moved. The dated figures below are left as
> written; [the catalog](../designs/internal-extensions.md#re-measured-on-rebase-2026-08-05)
> reconciles the two measurements file by file.
>
> **Then down, for the first time: 46,797** across 235 files, when `guard-budgets`
> became the first internal extension and the engine moved to
> `tools/internal/budget`. The downward move is the half that was still unproven —
> and `exact: true` is what forced it to be recorded, since extracting the code
> and leaving this line alone fails with `SHRANK — LOWER IT` and the new number.
>
> **And again: 46,106** with `guard-docs`, **45,763** with `posture-at-rest`, and
> **45,229** with `assert-storage`, **44,826** with
> `reconcile-actions` **44,171** with `teardown`, **43,817** with `template-sustain` and **40,827**
> with `import-brownfield` and **38,821** with `obj-encryption`, **38,364** with `guard-charts` and
> **37,483** with `cluster-access`, **37,131** with `health-sla` and **36,107** with
> `token-inventory`, **34,359** with `converge` and **33,877** with `assert-platform`.
> and **33,157** with `assert-reconciler`.
> and **32,965** with `assert-registry`.
> and **32,733** with `promote-pipeline`.
> and **32,077** with `posture-credential-coverage`.
> and **31,372** with `config-readiness`.
> and **30,687** with `env-topology`.
> and **29,853** with `assert-network`.
> and **29,450** with `wave-health`.
> and **29,230** with `tofu-driver`.
> and **27,156** with `assert-observability`.
> and **26,174** with `assert-secrets`.
> and **25,274** with `assert-identity`.
> and **25,020** with `deliver-docs`.
> and **24,807** with `argocd-diagnostics`.
> and **24,203** with `posture-plaintext`.
> and **23,898** with `chart-publish`.
> and **23,653** with `guard-manifests`.
> and **23,387** with `assert-objstore`.
> and **23,205** with `wedge-gameday`.
> and **22,964** with `phase-timing`.
> and **22,726** with `doctor-probes`.
> and **22,566** with `kyverno-policies`.
> and **22,383** with `managed-fresh` (which grew `template-sustain` rather than adding an extension).
> and **22,153** with `dev-mutation-testing`.
> and **21,841** with `release-publish`.
> and **21,542** with `credential-state-passphrase`.
> and **21,457** with `internal/baoread` (a shared package, not an extension).
> and **21,082** with `credential-pat` + `credential-objkey`.
> and **20,835** with the rotation table (wall three of the credential family).
> and **20,591** with broad-PAT + temp-objkey.
> and **20,492** with wall four half down.
> and **20,269** with wall four finished for the credential family.
> and **19,436** with `database-provisioner` + `assert-database`.
> and **19,074** with `openbao-seed`.
> and **19,006** with `openbao-peer-ca`.
> Forty-five extensions, net −28,176 (59.7%) — now BELOW the
> 41,803 this gate first recorded, and below the pre-rebase number — a floor on
> the effort rather than a schedule, since the cheapest went first. The catalog's
> [closure census](../designs/internal-extensions.md#the-cost-of-the-interesting-half)
> measures what the rest costs and finds size and difficulty close to uncorrelated.

`exact: true` makes "no
slack" literal in both directions: the gate fails when the number sits ABOVE the
measurement too, so a change that shrinks package `main` cannot bank the
reduction as undeclared slack for the next change to grow into — which is how a
high-water mark silently decays back into a ceiling with headroom, at exactly the
moment decomposition starts working.

That is a deliberate departure from `.untestable-budget.yaml`'s +~3% convention,
and measurement forced it. Package `main` grew 13,328 → 41,658 lines between
2026-06-16 and 2026-08-03 — ~590 lines/day, sustained over seven weeks and
accelerating. Every headroom is the wrong number against that:

| headroom | runway | problem |
|---|---|---|
| +3% (~1,200) | **2 days** | fires almost at once, on an unrelated PR whose author cannot pay a multi-day refactor — so it gets raised away |
| +15% (~6,300) | 10 days | still fires soon, and now sits 2× above the ~3,000 target this ADR argues for |

Decomposition cannot outrun it either: the catalog's whole first five (8,068
lines) buys 14 days, and extracting *everything* it identifies as decomposable
(37,945) buys 64. **A ceiling with slack is the wrong instrument for a metric
moving this fast** — which is why the first draft of this ADR, which set +3% and
claimed the gate would "bite on the next accretion", was wrong about its own
mechanism.

So there is no slack, and raising the number is expected rather than forbidden.
Any change that grows package `main` fails until its author updates that line in
the same commit. The increase then appears as a reviewable line in the diff, next
to the code that caused it — which is the "conversation at review time instead of
a number nobody looks at" this ADR asks for, now happening by construction rather
than by anyone remembering. Paying it is mechanical and self-service, so the gate
never blocks someone who cannot act, which is what stops it being deleted. And it
still ratchets DOWN for free: move code to `tools/internal/<pkg>` or out to an
extension and the measurement drops, so the line drops with it.

What is forbidden is raising it in a commit that does not explain the growth. The
signal to watch is the trend of this line, not any single value.

**What reduces it:** extract to `tools/internal/<pkg>` (ADR 0013's direction and
the default answer), make it an extension (issue #10 — per the catalog that is the
same move with a registry and a binding attached, since 36 of 57 candidates must
stay in-process Go), or delete what is dead. The gate says as much on a breach;
the wording lives in `budget.CoreSurfaceRemedy` and is not restated here.


### Where the budget is going

A ceiling with no destination is just a speed limit. The destination comes from
[the catalog](../designs/internal-extensions.md), which assigns every file in
package `main` exactly once and finds **91% of it decomposable**, leaving a
settled residue of **~2,900**. So the honest target is **~3,000 logic lines**, and
the number in `.core-surface-budget.yaml` is not that target — it is wherever the
code currently sits, which is why it carries no slack.

The catalog also sizes the first step and shows that universality is not the
discriminator — 34 of its 57 candidates ship always-enabled, so a framework
carrying only the optional ones relieves a third of what one carrying all of them
would. Those numbers live there. This ADR's position is only that the line should
be dragged toward ~3,000 rather than defended wherever it stands.

### Why a second config and command, not a category in `.untestable-budget.yaml`

Because the two gates push in **opposite directions**, and a shared error message
would be actively wrong. untestable-loc's remedy is "move the logic into
`tools/cmd/llz`". This gate's remedy is "move it out". A single file whose
breach message says both is not a gate, it's a riddle.

So they share the *engine* and not the *doctrine*: one scan, one glob walk, one
tally, one ratchet convention, in `tools/internal/budget`. The only thing this ADR
made configurable is the remedy sentence. The two budgets are then reviewed and
ratcheted independently, which is what they need — they will often move in
opposite directions in the same PR, and that is exactly the trade being made
visible.

### What it deliberately does not measure

- **It is a surface budget, not a quality metric.** Moving a file to
  `tools/internal/` satisfies it without deleting a line. That is intended: the
  move *is* ADR 0013's direction, and the package boundary is the point.
- **Test files are excluded outright.** Tests are what this repo wants more of;
  charging for them would price the wrong behaviour.
- **`tools/internal/` is not counted.** Code that got there has already done
  the thing this gate asks for.
- **It cannot tell good growth from bad.** A genuinely core capability that
  cannot be decomposed will eventually need a raise. The doctrine is that such a
  raise is argued for in a comment in the config file, with reasoning — the way
  `.untestable-budget.yaml` records its own — not applied silently to go green.

## Corollary: one ownership authority

The outward path (issue #10) needs to fence `copier update` off files an
extension owns, so an upgrade does not re-render and 3-way-merge them onto the
instance's own content. PR #15 implements that as a **second** fence: a runtime
`copier update --exclude` built from the extension lock, via a variadic
`copierUpdateArgv(ref, excludes...)`.

This repo already has that authority, and only one is allowed. `.template-manifest`
classifies every scaffold file `managed` / `merge` / `owned`; the class table in
`ci_template_manifest.go` is the single definition of what each class *means*,
including `copierFenced` — and `checkCopierFencing` enforces it against
`copier.yml`. The header on that table says it outright: *"A NEW class is one row
here, not a fourth mechanism."* The digest lock was already collapsed into being
a projection of the manifest rather than a second list, for exactly this reason.

**A framework contributing owned paths declares them through the manifest, not
through a second fence.** Two fences can disagree; the whole point of the class
table is that there is nowhere for them to disagree.

To make that checkable rather than aspirational, `checkCopierFencing` now
enforces the containment in **both** directions. It already failed when a
`copierFenced` file was missing from copier's `_skip_if_exists`/`_exclude`. It
now also fails when copier fences a scaffold file the manifest does *not* class
as fenced — which silently pinned `merge`-class files at their first-rendered
version, since copier skips them and no restore pass puts a newer version back.
Both directions are green on `main` today; the reverse check is a ratchet against
a second fence appearing, not a bug fix.

## Consequences

- A PR that adds a `ci_*.go` now has to fit under a ceiling or lower it. That is
  a conversation at review time instead of a number nobody looks at.
- `make lint` runs both budget gates from one changed-file condition (either a Go
  or a workflow change fires both). They are pure Go over a config file and take
  well under a second each; splitting the condition would have cost recipe lines
  in a category with one line of headroom left, to save nothing measurable.
- **The first thing this gate fails is PR #15 — the extension framework itself,
  measured against its own rebase base (`5b5da683`, 2026-07-30):**

  | | base | with PR #15 | Δ |
  |---|---|---|---|
  | Go logic lines in package `main` | 35,254 | 38,729 | **+3,475** |
  | non-test files | 189 | 210 | **+21** |
  | `ci_*.go` files | 99 | 99 | **0** |

  Against a high-water mark that carries no slack, that is a breach on the first
  line — which is the point: #15 would have to state its +3,475 explicitly. The
  framework built to relieve core overload currently *adds* to it: it compiles its
  ~30 `extension_*.go` files into package `main` and moves no core Go out (the
  built-ins it migrated were workflow config and lint YAML, which is why the
  `ci_*.go` count does not move).

  This is the gate working, not the gate obstructing. It converts "this PR is
  large" into a number and a specific ask: land the framework in
  `tools/internal/extension` behind a package boundary, and let the capabilities
  it migrates come out of the core rather than alongside it. A relief valve that
  is itself plumbed into the core relieves nothing.

  **Phase 1 of that ask is in this change.** `tools/internal/extension`
  (docs/designs/internal-extension-model.md) is the declaration model — states,
  bindings, grants and their validation — behind a package boundary, wired to
  nothing, costing zero core surface. It replaces PR #15's `kind: check|tool`
  ceiling, which banned `→ seeded` by omission; the ceiling is now a relationship
  between what an extension binds and what it may touch, so a refusal comes with
  a reason. The action ABI is deliberately absent until `converge` and
  `import-brownfield` force its shape.

  The catalog sharpens the rest of that ask into an ordering. PR #15 front-loads
  the **remote** half — git-pinned `sources:`, the digest lock, the trust model,
  `extension sync` — which can serve at most the 21 externalisable candidates and
  none of the 36 that need in-process Go. The half that unblocks 91% of the
  decomposition is the **internal Go action ABI**, and it is the half the spike
  treats as a later phase. Reversing that order is the single highest-leverage
  change available to #15.

- **Run the acid test second, not last.** The catalog's most valuable single
  split is inside `converge`: `health.go` (1,097 lines) fuses the *action*
  with the *predicate*. Separating them — health becomes the core-registered
  `converged` assertion, converge stays the extension action — is where the
  binding model either holds or doesn't. Deferring it risks building a registry,
  a loader and a lock around a model that does not survive its first hard case.

- **Two structural findings worth settling before the count is fixed.** Several
  capabilities pair with their own assertion (`harbor-provisioner` ↔
  `assert-registry`, `database-provisioner` ↔ `assert-database`,
  `keycloak-provisioner` ↔ `assert-identity`, `reconciler-runtime` ↔
  `assert-reconciler`) and turn on and off together, which argues for one
  extension carrying both bindings; that alone moves the count by ~8. Pulling the
  other way, `token-inventory` (three states) and `reconcile-actions` (seven
  invariants) want splitting. The real shape is ~45–55 internal extensions, which
  is a registry sized for dozens, not for the handful the spike assumes.

- Measured over four days at the end of July 2026, package `main` went 189 → 213
  non-test files, and over the five weeks to 2026-08-05 it went 213 → 236 with
  nothing extracted. That is the rate this budget is calibrated against, and it is
  why the number carries **no** headroom: at ~590 lines/day any margin large enough
  to avoid nuisance failures is also large enough to pre-approve a week of
  unexamined growth.
