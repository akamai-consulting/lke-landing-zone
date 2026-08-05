---
name: gate
description: Choosing and writing the gate that proves a behavior still works. Use BEFORE implementing any behavior change — a new assert lane, a static guard in make lint, or a coupling test between two components — and when answering the AGENTS.md "name the gate" requirement in a PR body. Encodes the two regressions that shipped green and the doctrine that came out of them.
---

# Gating a behavior

`AGENTS.md` requires it in one line: **new behavior ships with a gate that fails
when the behavior stops working.** Not a test that the code renders, parses, or
is present.

[`docs/e2e-gates.md`](../../../docs/e2e-gates.md) is canonical — the two
regressions, the archetypes, the doctrine. Read it. This file is the *procedure*:
the questions to answer in order, and the wiring that has its own traps.

## Step 0 — is a gate even the right answer?

Be honest, or the convention gets ignored. A refactor with no behavior change
does not need one. A doc change does not. A statically decidable invariant
belongs in `make lint`, where it costs seconds instead of a cluster.

It **does** need one when any of these is true:

- a component starts depending on another component's **live output**
- something is renamed or reformatted that a **second component parses**
- the failure mode is **silence** — nothing errors, nothing restarts, nothing alerts

If none apply, write which and why in the PR body. That is a valid answer. "None"
with no reason is not.

## Step 1 — which archetype

Almost every gateable behavior is one of two shapes, and the shape decides the
gate.

**Unverified delivery** — A is configured to send to B. A push URL, a scrape
target, a webhook, a secret written to a store another component reads.

> Gate **at the consumer, on data the producer actually emitted**. Not "is the
> config right" — it looked right. Not "is B healthy" — `assert-loki` was green
> the entire time the audit pipeline shipped nowhere.

**Split contract** — A and B share a rule, each holding its own copy. A naming
scheme, a label format, a path layout, a truncation limit.

> Feed the producer's **real** output into the consumer's **real** predicate. Never
> restate either rule. `TestReaperRecognisesRelabelerOutput` calls the relabeler's
> actual `desiredVolumeLabel()` and hands each result to the reaper's actual
> `linode.VolumeIsCandidate()`. A test that re-implements the predicate passes
> happily while the real consumer goes blind.

## Step 2 — which layer

| The behavior… | Gate it with |
|---|---|
| is decidable from repo contents alone | a static guard in `make lint` |
| is a contract between two components here | a coupling test calling **both sides' real functions** |
| only exists once something is running | an `llz ci assert-*` verb in the suite |

Pick the cheapest layer that can actually **see** the failure — and be honest
about what each cannot see. The audit-pipeline regression was invisible to the
first two by construction: two values consistent with each other cannot be told
from two correct values without asking a cluster.

Most gates want **both halves**. Keep the static half beside the live one, so a
future divergence fails at PR time rather than at e2e time.

## Step 3 — hold the gate to the doctrine

These are the arms that decide whether the gate is real. Walk them one at a time;
this is where the first cut of a lane usually fails.

- **Fail closed on vacuity.** Zero streams, zero targets, zero PVs, an
  unreachable API — all failures. A gate that reports success having examined
  nothing launders an absence of evidence into a green check, and "examined
  nothing" is exactly what a broken pipeline looks like.
- **Separate "could not tell" from "nothing there."** A parse failure or an
  unreachable endpoint is an error, not an empty result.
- **Assert freshness, not existence.** Loki retains history: "some audit line
  exists" stays green for the whole retention period after the pipeline breaks.
  Bound the query to a window and require the newest entry inside it.
- **Never derive the expected set from the thing under test.** If the label you
  filter by is the thing that regresses, a filtered query returns an empty
  expected set and the gate passes on the bug it exists to catch.
- **Prefer ground truth to a proxy.** "Is every PVC on `block-storage-retain`?"
  infers encryption from a StorageClass *name*; that proxy was in place while 13
  of 16 PVCs provisioned unencrypted.
- **Pin the exclusions.** A destructive selector must keep saying *no*. A gate
  proving only "matches more" is one edit from matching everything.
- **Bound the settle budget.** Poll for a stated budget, then fail. Do not retry
  forever and do not assert the instant converge returns.
- **Distinguish absent from not applicable.** Read live state and skip on it —
  `assert-network-enforcement` reads the namespace's PeerAuthentication mode
  rather than assuming STRICT, so it starts enforcing the day someone flips it.
  A gate that cannot tell a not-installed component from a regression gets turned
  off, which costs more than the coverage it protected. **But if every check in a
  lane skipped, fail.**

## Step 4 — write the failure message the reader needs

Nine of fourteen lanes went red the first time the suite met a real cluster. The
cheap ones named the specific missing thing; the expensive ones said something
true and useless and sent the reader to a live cluster.

- **Name what IS present**, not only what is missing. The ref being looked for
  had been *renamed*, and no amount of staring at the absent name reveals the new one.
- **Print the parameter you queried with.** "Collection stopped" and "we asked the
  wrong tenant" are indistinguishable from the bare symptom and have nothing in
  common as remedies.
- **Keep the distinction your probe already made.** `llz ci net-probe` classifies
  every dial as refused / timeout / dns because they point at different
  subsystems; collapsing them to "blocked" throws that away.

## Step 5 — wiring a live lane

1. **Write the verb** as `tools/cmd/llz/ci_assert_<thing>.go`, registered in
   `ci.go`. Keep the judgement in a **pure function over parsed input** so it is
   testable without a cluster; keep the transport (kubectl, port-forward, API) in
   a **seam a test can replace**. `ci_assert_scrape.go` and
   `ci_assert_openbao_audit.go` are the models.
2. **Unit-test it** — the pure evaluator, every fail-closed arm (empty,
   malformed, unreachable), and the static half of the contract.
3. **Add the lane** to `assertSuiteLanes` in
   [`tools/cmd/llz/ci_assert_suite.go`](../../../tools/cmd/llz/ci_assert_suite.go).
   One list, so a lane is both run and collected — the "declared but never
   checked" hazard is structurally gone. Fill in every field:
   - `Steps` run in order and short-circuit. Order them only when a later step
     depends on a fact an earlier one just made true (see `scrape-reconciler`),
     otherwise give them their own lane so they run in parallel.
   - `Gating: true` unless the lane is a diagnostic whose *output* is the
     deliverable. `Gating: false` means the exit status is discarded.
   - `Why` — what the lane proves **and what stays green without it**. It prints
     with the lane's group, so a failure carries its own rationale.
4. **Check lane safety:** mutating lanes must touch disjoint namespaces, and
   anything port-forwarding binds local port `:0`.
5. **Document it** in `docs/workflows/llz-bootstrap-openbao.md` — one bullet.

Verify the table without a cluster:

```bash
cd tools && go run ./cmd/llz ci assert-suite --list
```

The suite ships **in the binary**, not in each instance's vendored YAML — which
is why a new gate reaches every instance on the next release rather than when
someone remembers to edit their workflow.

## The two regressions, in one line each

Keep these in mind while writing; they are the shape of what gets missed.

- **Two wrong values that agree with each other look correct.** The audit push URL
  and the NetworkPolicy allow named the *same* nonexistent namespace. Complete
  from every angle, granting nothing. No static check can distinguish a consistent
  pair from a correct pair.
- **A rename on one side of a contract.** The commit that started renaming volumes
  edited the reaper's own file, ~19 lines from the prefix check it invalidated.
  Both sides had passing tests; each tested its own copy of the rule.
