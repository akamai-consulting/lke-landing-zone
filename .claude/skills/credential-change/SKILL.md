---
name: credential-change
description: Adding, moving, rotating or retiring a credential. Use when introducing a new secret an instance workflow consumes, adding an OpenBao KV path, changing a rotation cadence, or when credential-coverage-guard / at-rest-guard / plaintext-guard / assert-rotation-health goes red. The fan-out is wider than it looks and the failure mode is a credential nobody measures.
---

# Changing a credential

A credential is never one edit. It has a **write path**, an **age**, a **class**,
a **policy grant**, an **at-rest story**, and a **transport** — and each has its
own gate. The recurring failure is not a leaked secret; it is a credential that
quietly falls off the single pane, where **no alert can fire for it, because an
alert on an absent series never evaluates.**

[ADR 0009](../../../docs/adr/0009-unmeasurable-credential-coverage.md) and
[ADR 0012](../../../docs/adr/0012-credential-observability-gaps.md) are the
decision records. [`docs/secrets.md`](../../../docs/secrets.md) is the operator
view.

## The precedent that shaped every gate here

ADR 0009 went looking for credentials with no expiry, found the state backend,
wrote three entries — and left `OPENBAO_SEAL_KEY`, the at-rest key for every
other credential in the platform, off the pane entirely. **Not by decision. By
omission.**

Adding a `secrets.FOO` to a workflow was a one-line edit no gate looked at. That
is what `credential-coverage-guard` now makes a reviewed one.

## The checklist

Work these in order. Most changes touch 3–4 of them.

### 1. Is it measured?

Every secret an instance workflow consumes must be **measured** by one of the
three feeds — `ghPATTargets`, `ghSecretTargets` (`ci_token_inventory.go`) or
`credPaths` (`reconcile_openbao.go`) — **or registered as an exemption with a
reason and an owner**.

`credential-coverage-guard` derives coverage from those feeds directly rather
than keeping its own copy, so the registry holds **only** exemptions. The exempt
reasons are a **closed vocabulary**: "accepted" is not a reason, and a free-text
field would collect one.

> **Unused entries fail.** A registry keeping entries for secrets no workflow uses
> stops being reviewable, because the next reader cannot tell which lines are
> load-bearing. Same rule for `plaintextAllowed`.

Scope note: the guard covers `instance-template/.github/workflows` — the surface
an **instance** runs on. The template repo's own harness workflows are out of
scope by design; pulling them in would put maintainer-only credentials on every
adopter's dashboard as permanent `unknown` rows.

### 2. Pick the class honestly

`credPaths` carries a class per path, and it answers exactly one question: **what
lowers this credential's age once it exists?**

| Class | Meaning |
|---|---|
| `automated` | a rotator resets it on a cadence — belongs on the 90d SLA |
| `generate-once` | created in-cluster with a generated value |
| `tracks-source` | mirrored from a source of truth outside OpenBao |
| `on-demand` | a real rotation path exists, but an **operator** triggers it |
| `static` | seeded once and never rotated |

`static` exists because those paths published **no series at all** and were
invisible on the pane rather than visibly old. Choosing it to silence an alert
re-creates the blind spot the whole subsystem exists to close.

**`optional` is orthogonal to class** and conflating them costs one property or
the other: a credential that is on a real SLA *and* seeded by only some instances
needs `on-demand` **and** `optional: true`. Demote it to `static` and it silently
loses the SLA the docs promise; leave it required and every stock cluster reds for
a credential that is correctly absent.

### 3. Grant the policy read — in the same change

> Every path in `credPaths` **must** also be granted a `secret/metadata/<path>`
> read in `policyReconcilerRead` (`ci_openbao_configure.go`).

A missing grant is a **403**, and a 403 is a non-404 error that fails the
**whole** sampler pass (`up=0`). It does not degrade to one missing series. These
two move together or the lane dies.

### 4. Retiring: retire the SERIES, not just the path

Deleting the credential without retiring its series leaves a gauge that ages
forever and pages on a credential that no longer exists. A comment left in the
registry can also keep a dead exemption alive.

### 5. At rest and in transit

- `at-rest-guard` — encryption at rest. State encryption is an
  [ADR 0007](../../../docs/adr/0007-terraform-state-encryption.md) concern; note
  that enforced and fallback key modes are **mutually exclusive**, so a cutover is
  inherently two-phase.
- `plaintext-guard` — unencrypted in-cluster hops, and it sees cleartext that is
  not HTTP.
- `mtls-wiring-guard` — a pod declaring it reads OpenBao must actually **mount**
  what it reads. [ADR 0010](../../../docs/adr/0010-in-cluster-mtls.md) records the
  cutover; Harbor is **PERMISSIVE on purpose** at step 3, so a successful
  plaintext dial there is correct behavior, not a finding.

### 6. Know what nothing can see

A credential seeded by hand straight into OpenBao appears in no workflow, so
`credential-coverage-guard` **cannot** see it. That residue belongs to `credPaths`
and `assert-rotation-health`. Never read a green coverage run as "every credential
is covered".

## Verifying

```bash
make credential-coverage-guard
make at-rest-guard
make plaintext-guard
make mtls-wiring-guard
```

The live half is `llz ci assert-rotation-health`, which guards the three-component
contract — `credPaths` declares, the gauges lane samples, the alert rule fires —
whose native failure is a declared credential that publishes no series.

## Two platform constraints, not bugs

From [`docs/lessons-learned.md`](../../../docs/lessons-learned.md):

- **Secret rotation on LKE-E is `lke-admin-token` only**, via the delete-kubeconfig
  API. There is no sanctioned batch SA-token rotation. **Never `kubectl delete` the
  `lke-admin-token` Secret** — on LKE-E it is not regenerated.
- That rotation deliberately reuses the broad shared token rather than a scoped
  PAT. It is an accepted, documented deviation. Do not re-propose least-privilege
  for it.
