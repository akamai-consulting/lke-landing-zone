# Handoff — extension framework audit findings

Audit performed against local `feat/first-internal-extension` @ `9b8b518e`.

**Read items 1–3 together before starting.** They are one story: the registry gate
driver was built, has two defects, and is wired to nothing — so the defects are
latent today and become live the moment someone wires it. Fix 2 and 3 *before* 1.

## Repo state you are inheriting

- Local branch is **12 commits ahead** of `origin/feat/first-internal-extension`
  (remote tip `5e5bcbd5`). Account for that before pushing.
- 61 declarations / 60 packages, grouped
  `tools/internal/extensions/{assertions,guards,lifecycle}/`.
- Framework, production lines only — note `grep -v _test` filters LINES not FILES;
  use `ls *.go | grep -v '_test\.go$'`:
  `shared/extension` 923, `shared/capability` 1189, `registry` 811,
  declarations 4360.

### How the gates actually run today

Each gate is its own Makefile target using the `LLZ_CI` macro (`Makefile:452`),
which does `cd tools` and then invokes the gate's own `llz ci` verb with
`--root ..`. That is why `--root ..` is correct: it resolves to the repo root
**from `tools/`**.

(docs-guard rejected two earlier drafts of that sentence: writing the invocation
with a placeholder verb makes it resolve to the bare parent command, which has no
`--root` flag, so the guard reports an undefined flag. Then the sentence
*explaining* that did it again. A small live demonstration of the check item 2
says the driver disables — and of why it is worth keeping.)

`llz ci gates` — the registry-driven runner — is a *second, parallel* path that
nothing invokes. See item 1.

---

## 1. The registry gate driver is wired to nothing

**Severity: high (as a gap between claim and reality).**

`d7987336` introduced `llz ci gates` citing issue #399's Phase 2 acceptance
criterion:

> "a gate binding RUNS FROM THE REGISTRY, not from a hardcoded call. Without that
> this is a directory, not a framework."

The command exists and works. **Nothing calls it.** Exhaustive check:

```
grep -rn 'ci gates\|GatesCmd\|RunGates' --include=Makefile --include='*.yml' \
     --include='*.yaml' --include='*.sh' .
# → only .core-surface-budget.yaml:1078, a comment ABOUT it
```

The only Go references are the definition (`registry/gates.go`) and the
registration (`cmd/llz/ci.go`). CI and `make lint` still run the gates through
their individual Makefile targets.

So the acceptance criterion is met in the sense that the code *can* run from the
registry, and unmet in the sense that nothing does. Decide which you want:

- **Wire it** — replace the individual Makefile gate targets with `llz ci gates`.
  **Do items 2 and 3 first**, or you ship a silently weaker `docs-guard` into CI.
- **Or state plainly** that it is a demonstration of the model's driveability and
  not the production path, and record that in the issue and the package doc.

Either is defensible. What is not defensible is leaving a second gate path that
looks authoritative, is never exercised, and drifts from the one that runs.

---

## 2. Under the driver, `docs-guard` silently skips its largest check

**Severity: high, currently LATENT** (see item 1 — the driver never runs in CI).
This must be fixed before item 1 is wired.

### Mechanism (established from the resolution logic, not inference)

`docs-guard` validates documented `llz …` invocations against `cmd.Root()` — the
*live* cobra tree. `docsguard.go:384` starts at `cur := rootCmd` and descends with
`cur.Find(...)`; `n.Invocations++` happens **only after `words > 0`**, i.e. only
once a real command resolved.

`RunGates` (`registry/gates.go:202`) constructs a **parentless** command:

```go
c := g.New()          // no parent
c.SetArgs(g.Args)
c.Execute()
```

A parentless cobra command's `Root()` returns itself. So `rootCmd` is a tree
containing only `docs-guard`; `cur.Find(["ci"])` fails for every documented
`llz ci …`; `words` stays 0; every invocation hits `continue` and is skipped —
along with every flag check that depends on a resolved command.

### Evidence

Same cwd, same `--root ..`, same 128-file corpus located both ways:

```
cd tools
llz ci docs-guard --root ..           # checked 862 llz invocation(s) / 247 flag(s)
llz ci gates | grep docs-guard        # checked   0 llz invocation(s) /   0 flag(s)
```

The corpus is found either way (128 Markdown files in both), so this is the root
resolution and not a path problem.

> An earlier draft of this handoff "proved" it with a throwaway test inside the
> `registry` package. That test ran with `registry/` as cwd, so `--root ..`
> pointed somewhere else entirely, and it swallowed the error. It proved nothing.
> The two commands above, and the loop at `docsguard.go:384`, are the evidence.

### Constraint on any fix

`docs-guard` needs the **real** llz tree — checking docs against commands that
actually exist is the point. The root is already available at the call site:
`RunGates` is called from `GatesCmd`'s `RunE`, which receives `c *cobra.Command`
(`gates.go:245`). It simply is not threaded through.

Only `docs-guard` has this dependency — verified across all six driven-gate
packages (`budget`, `chartguard`, `credcoverage`, `docsguard`, `plaintext`,
`wavehealth`); the other five use no cobra context.

### Options (pick one, argue it in the commit)

- **(a) Thread the root through.** `RunGates(root *cobra.Command, …)`, and
  `root.AddCommand(c)` / `root.RemoveCommand(c)` around `Execute`. Smallest
  change; temporarily mutates the live tree (nothing else walks it concurrently).
- **(b) Declare the dependency on `Gate`.** e.g. `New func(root *cobra.Command) *cobra.Command`,
  or a `NeedsRoot bool`. Eight table entries. Makes the requirement explicit,
  which matches the driver's own thesis that requirements should be declared
  rather than assumed. **My preference.**
- **(c) Remove the coupling from `docs-guard`** — inject the tree through an
  explicit seam. Largest change; only if you think `cmd.Root()` is wrong in the
  first place, which the package header (`cobra_docs_guard.go:9`) argues against.

### Verification

`docsguard.Report.Scanned.Invocations` is exported and usable. Add a regression
test in `registry/gates_test.go` asserting the driver-run `docs-guard` reports
`Invocations > 0` — **run it with `tools/` as the working directory**, or
`--root ..` will resolve to the wrong tree and the test will pass for the wrong
reason.

---

## 3. The driver hardcodes `--root ..` and will scan sibling repositories

**Severity: high. This one I got wrong in the previous draft** — I called it "a
cwd artifact, not a defect". It is a defect.

`registry/gates.go`'s table passes `--root ".."` to every gate, with no check that
`..` is the repo root. Run from the repo root (the natural place), `..` is the
*parent directory of the repo*:

```
llz ci gates            # from the repo root
# docs-guard: 322 finding(s) across 907 file(s)
```

Those 907 files span at least six sibling checkouts on the developer's machine:

```
47  lke-landing-zone-example/     27  lke-landing-zone-pf/
39  lke-landing-zone/             27  lke-landing-zone-instance-cat/
27  lke-landing-zone-cc-deadlock/ 27  lke-landing-zone-beta/
```

It reports findings against repositories it was never pointed at. The Makefile
path avoids this only because `LLZ_CI` does `cd tools` first — an invariant the
driver assumes and never states or checks.

### What to do

Make the driver resolve the repo root rather than assume its cwd — e.g. locate it
by walking up for a marker (`go.mod`, `.git`, `.core-surface-budget.yaml`) and
fail loudly if it cannot. A gate suite that silently changes its subject based on
where it was invoked is worse than one that refuses to run.

---

## 4. Make the gate driver fail closed on an empty corpus

**Severity: high. This is the general form of items 2 and 3 — it would have caught
both.**

`plaintext-guard` and `wave-dependency-guard` already refuse an empty corpus:

> "A guard that had nothing to check reports the same green as one that checked
> everything, so this fails instead."

`docs-guard` does not — it reported `0 llz invocation(s)` and returned nil. That
asymmetry is exactly why item 2 was invisible.

Decide where the rule lives:

- **In the driver** — treat a zero corpus as failure. Needs gates to report what
  they examined across the cobra boundary; `docsguard.Report` already carries
  `Scanned`/`Read`/`Total`, but only stdout crosses today.
- **In each gate** — extend the `plaintext-guard` pattern. Simpler, but a
  convention the next gate will forget. If you choose this, add a test that every
  driven gate fails on an empty corpus.

Whatever you pick must hold for the 12 undriven gates when they are driven —
`registry/gates_test.go`'s `TestUndrivenGatesAreNamedInTheSource` names them.

---

## 5. `read-repo` is the largest grant and constrains nothing

**Severity: medium. Design work, not a bug.**

```
read-repo      40   NO handle          cloud-mutate   16   Forge
cluster-read   23   Cluster            secret-custody 12   Custodian / Forge
cluster-write  16   Writer             secret-read     9   Secrets
cloud-read     16   NO handle          write-repo      2   NO handle
                                       own-paths       1   NO handle
```

Five grants now resolve to real handles (`tools/internal/shared/capability/`:
`capability.go`, `writer.go`, `secrets.go`, `forge.go`, `baoadmin.go`).
`read-repo` is declared more than any other and means nothing at runtime —
conspicuous *because* the others got teeth.

Two directions, both previously argued:

- **Give it a handle** — a repo-reader fenced to the instance tree. Would also make
  `write-repo` meaningful, and would let the driver enforce its "a gate may hold
  read-repo and nothing else" claim structurally rather than by validator check.
- **Split it** — reading the spec, reading the tree, and reading
  `.template-manifest`'s class table are materially different permissions.

**Measure before designing.** Every capability handle here was sized by counting
call sites first, and every time the raw count was an upper bound: 26 exec seams
turned out to be 3 shapes; the "six" writer operations turned out to be 8, but
only because the first census missed everything that bypassed a seam. Find what
`read-repo` holders actually read before deciding whether it is one capability or
three.

---

## Explicitly NOT recommended

**Do not re-file `tools/internal/extensions/assertions/`.** An earlier pass of this
audit recommended moving the 7 packages there that hold a `Transition` or
`Invariant`. **That recommendation was wrong and is withdrawn.** All seven are
assertion-*primary* with one small mutating half:

```
assert-identity       Assertion + Transition
assert-network        Assertion ×4 + Transition
assert-observability  Assertion ×2 + Transition
assert-platform       Assertion ×4 + Transition
assert-secrets        Assertion ×3 + Transition
token-inventory       Assertion + Gate + Invariant
assert-storage        Assertion + Invariant ×2
```

That is the shape the model explicitly endorses (`extension.go:83`): "a capability
and its assertion … enable and disable together, which argues for one extension
holding both bindings rather than two that must be kept in step by hand." Filing
`assert-network` under `lifecycle/` because it tears down its own probe namespace
would be worse than what is there.

Carry the lesson: **a count of binding kinds is not a claim about what an extension
is.** Look at the shape.

---

## Left open deliberately

- **`Always` is not a toggle.** 54 always / 7 opt-in; `.Always` is read in two
  places, one being the list renderer. Cheapest *after* the driver generalises — a
  registry that can run bindings is most of what a loader needs.
- **`registry.Validate()` / `ValidateSet` are test-only.** The ceiling is a CI lint,
  not a runtime constraint. Defensible; amend the design doc to say so rather than
  implying otherwise.
- **12 raw `exec.Command("kubectl", …)` sites**, ratcheted by
  `TestNoNewRawKubectlExec` (`shared/capability/rawexec_test.go`), which fails in
  both directions. Three are long-lived `port-forward` tunnels and one is an
  interactive `kubectl exec`: they return processes, not bytes, and do not fit the
  `Writer` shape. Closing them needs a session-handle abstraction — a real design
  question, not more of the same conversion.

## Gates you must pass

```
cd tools && go build ./... && go vet ./... && go test ./... -count=1
cd tools && staticcheck ./...
make coverage        # per-package floors; raise, don't lower, unless code MOVED out
make instance-test
make lint            # the gates as CI actually runs them
llz ci core-surface  # exact:true — update .core-surface-budget.yaml in the SAME commit
```

`core-surface` fails in both directions: if package main shrinks you must lower the
budget in the same commit, with the reason in the ledger prose.

Prefer `make lint` over `llz ci gates` for verification until item 3 is fixed —
`make` guarantees the `cd tools` the driver assumes.
