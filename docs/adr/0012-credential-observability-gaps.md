# ADR 0012 — Closing the credential-observability gaps, and gating them shut

Status: **accepted**, implemented.
Date: 2026-07-30
**Extends** [ADR 0009](0009-unmeasurable-credential-coverage.md) (credential-age
coverage), [ADR 0007](0007-terraform-state-encryption.md) (state encryption at
rest) and [ADR 0010](0010-in-cluster-mtls.md) (in-cluster mTLS). It supersedes
none of them; each was correct about the mechanism and wrong about how much of
the surface the mechanism was actually applied to.

## Context

A review of four questions — is every key and token *monitored*, is every one
*rotated*, does anything still cross the pod network *in cleartext*, is
everything encrypted *at rest* — found that the platform's answers were already
built. Three ADRs had designed the mechanisms, shipped them, tested them, and
written them up. What none of them had was a **forcing function**, and in every
one of the four areas the gap turned out to be the same shape:

> A literal list in Go, written once by whoever built the mechanism, never
> revisited, and gated by nothing.

That shape produces a specific and nasty failure. A credential that is not in the
list does not appear *wrong* on the dashboard — it does not appear **at all**, and
a Prometheus rule over an absent series never evaluates. The single pane looked
healthy in exactly the case where it had nothing to say.

### What was actually found

| # | Finding | Why it survived |
|---|---|---|
| 1 | **The write-time probe never ran.** `credential-single-pane` exported a token but no `GH_REPO`, so `newSecretAgeWriter` errored on every run since ADR 0009 shipped. Not one write-time series was ever published. | It fails soft by design — correctly — but soft-failed as a `::warning::` in a scheduled job's log. |
| 2 | **Three of nine credentials measured.** `OPENBAO_SEAL_KEY`, `OPENBAO_RECOVERY_KEY_1/2/3`, `OPENBAO_ROOT_TOKEN`, `HARBOR_PASSWORD`, `HARBOR_PULL_PASSWORD` have no expiry and no OpenBao home, exactly like the state backend. | ADR 0009 went looking, found the state backend, wrote three entries. Omission, not decision. |
| 3 | **An unconfigured credential published nothing.** The reconciler `continue`d before publishing any series when `UpdatedAt` was empty. | ADR 0009 argued no absence alert was needed because "the API distinguishes them". It does — and the distinction was carried into the inventory JSON and then dropped one layer later. |
| 4 | **The rotation gate covered one feed.** `assert-rotation-health` derives from `credPaths`, so the entire GitHub write-time lane was ungated. | The gate was written when that lane did not exist. |
| 5 | **A defaulted scrape scheme was invisible.** A ServiceMonitor endpoint with no `scheme:` key scrapes over HTTP; `plaintext-guard` matched only `scheme: http`. | Every other pattern in that guard reads a decision somebody typed. The cheapest way to add a plaintext scrape is to type nothing. |
| 6 | **No gate on at-rest levers.** `terraform { encryption }`, `disk_encryption`, `linode_volume.encryption` — all set correctly today, all absent-by-default, none asserted. | `tf-validate`, `tflint` and `checkov` have no opinion about a block that is simply not there. |

Findings 5 and 6 are **latent** — the tree is clean today. That is stated
explicitly rather than glossed, because it changes what the work is: those two are
drift gates on a correct tree, and a clean corpus is the cheapest moment to add
one, not a reason to skip it.

## Decision

**Measure what the platform holds, publish presence as its own signal, and put a
registry gate in front of each of the four surfaces so the next gap is a review
comment instead of an omission.**

### 1. Presence is a first-class signal, and it is not uniformly good

`llz_credential_configured{cred,class,expect}` is published for **every**
credential in `ghSecretTargets`, including ones the API reports absent. That alone
closes finding 3.

`expect` is a **label** with three values, not a filter, and that is the
load-bearing choice.
`OPENBAO_ROOT_TOKEN` is supposed to be **absent**: bootstrap mints a root token,
uses it, and revokes it, leaving the 3-of-5 recovery quorum as what survives. A
set one is a live, unexpiring, full-admin OpenBao credential left behind by a
break-glass whose revoke half never ran. Two rules therefore read the same series
in opposite directions:

- `LLZCredentialUnconfigured` — `expect="present"` and the value is 0
- `LLZCredentialRootTokenParked` — `expect="absent"` and the value is 1

A rule set that could only say "configured is good" would have had to leave the
root token unwatched. The promtool group pins that the two matchers cannot be
collapsed into one.

The third value, `optional`, matches neither rule. The Harbor robot pair is
published by the **active** peer's provisioner, so a standby peer legitimately
has neither until it has run — this ADR's first draft classed both `present`,
which would have paged every healthy standby and failed its daily credential job.
A gap closed by a rule that cries wolf is not closed.

**And a refused read is not an absence.** `llz_credential_configured` is
published only when the GitHub API actually answered — `ok`, or a 404 that says
the credential is not there. A 403 publishes nothing and drops the funnel gauge
instead. This is the same conflation as finding 3 one level down, and it was
live in this ADR's own first implementation: `gatherSecretAges` recorded both a
404 and a 403 as `unknown`/no-timestamp, and the reconciler turned that into
`configured=0`. Since `LLZCredentialUnconfigured` reads 0 as "go and seed this
credential", a token-permission fault would have paged while naming the wrong
thing — and the exposure is not hypothetical, because the five OpenBao
credentials added here are `infra-<region>` **environment** secrets, whose
metadata needs different permissions from the repo-scoped ones, on a code path
that had never run in production even once.

### 2. A funnel that cannot run says so

`llz_credential_secret_probe_ok` carries the writer's own verdict. An empty
`Secrets` list is ambiguous — "measured, none exist" and "the probe never
authenticated" render identically — and the ambiguity was not theoretical: it hid
finding 1 for the entire life of the mechanism.

Deliberately **not** expressed as `absent(llz_credential_age_days)`. Absence
cannot distinguish a broken probe from a deployment that genuinely holds none of
these credentials, and guessing wrong in either direction is worse than asking the
writer, which knows.

### 3. Four registry gates, all on the same terms

| Gate | Asserts | Registry |
|---|---|---|
| `credential-coverage-guard` | every `secrets.NAME` an instance workflow uses is measured | `credCoverageExempt` (kind + reason) |
| `at-rest-guard` | every TF root encrypts state; every node pool / volume sets disk encryption | `atRestAllowed` (reason + **exit condition**) |
| `plaintext-guard` (extended) | a scrape endpoint declares a scheme | `plaintextAllowed` (reason + owner) |
| `assert-rotation-health` (extended) | the write-time lane is live and every credential is configured as expected | derived, no registry |

Each shares the rules the plaintext guard established: **unregistered findings
fail**, **unused entries fail** (a registry that keeps dead lines stops being
reviewable), and **an empty corpus fails** (a guard that read nothing prints the
same green as one that read everything).

**Coverage is derived, never restated.** `credMeasuredByName` reads
`ghPATTargets` and `ghSecretTargets` directly. A second copy would drift from the
first, and a coverage gate that disagrees with the thing it gates is worse than no
gate — it would vouch for credentials nobody measures. The only way to satisfy the
guard for a real credential is to actually measure it.

### 4. Registries carry exit conditions where the residue is temporary

`atRestAllowed` requires an `exit` field. ADR 0007 shipped a two-phase migration
and only phase 1 has happened — all four roots still carry
`fallback { method = method.unencrypted.migrate }`, which is what lets OpenTofu
read pre-encryption state and also what makes an unencrypted state file *accepted*
rather than refused. That was a comment repeated in four files with no owner and
no test for retiring it, which is how a two-phase migration quietly becomes a
one-phase one.

One entry **per root**, not one shared entry: retiring it is per-root work, and a
shared entry would let the first migrated root vouch for the three that were not.

## Consequences

- **Six credentials joined the single pane**, including the one whose compromise
  reads every other credential in the platform (`OPENBAO_SEAL_KEY`).
- **The write-time lane actually runs.** One missing `GH_REPO`; everything
  downstream of it had been correct and inert since ADR 0009.
- **A parked root token is now a finding** with a one-dispatch remedy, in three
  places: an alert, the rotation gate, and the dashboard.
- **A new Terraform root cannot ship unencrypted.** `llz env add` scaffolds roots,
  so this is the realistic near-term regression, and it was the one no existing
  linter covered.
- **ADR 0007 phase 2 has an exit test** instead of four comments. It is still
  unfinished, and the registry says so in one place.
- **Four more gates in `make lint`.** Two run from source (`LLZ_FORCE_SOURCE`)
  because they compare the working tree against Go lists in the working tree, and
  the prebuilt image binary is built from the merge-base.
- **New false-page surface.** `LLZCredentialUnconfigured` fires on a credential an
  instance deliberately does not use. Mitigated by `expect` and by the exemption
  registry, but the remedy for a genuinely-unused credential is to remove it from
  `ghSecretTargets`, which is an edit rather than a silence.
- **The guards see workflows, not the world.** A credential seeded by hand
  straight into OpenBao appears in no workflow and so in no scan. That residue
  belongs to `credPaths` and `assert-rotation-health`, and is stated in the guard's
  own header so nobody reads a green run as "every credential is covered".

## What this does NOT do

- **It does not build rotation for the credentials that lack it.** The seal key,
  the recovery quorum and the Harbor robot copies are `static` because nothing
  rotates them, and classing them otherwise would page an operator who has nothing
  to dispatch — the same test ADR 0009 applied to the state passphrase. Rotating
  the seal key means a rewrap of OpenBao's entire raft store; that belongs in an
  ADR that argues it directly, and it is the honest residue of this one.
- **It does not flip ADR 0007 to `enforced`.** That needs every root's state
  migrated in every deployment, which is an observation this repo cannot make from
  source. The registry records the exit condition; making the flip is the next
  piece of work, not this one.
- **It does not close a single plaintext hop.** Finding 5 is a scanner gap, not a
  new hop; the accepted residuals in `plaintextAllowed` are unchanged and most of
  them close only under [ADR 0011](0011-ambient-mesh-migration.md).
- **It does not gate the template repo's own workflows.** Those run against a
  throwaway e2e instance, and pulling maintainer-only credentials into the scan
  would put permanent `unknown` rows on every adopter's dashboard.

## Alternatives rejected

| Option | Why not |
|---|---|
| Restate the measured set in the guard | A second list drifts from the first, and a coverage gate that disagrees with what it gates vouches for credentials nobody measures. Derivation makes that impossible by construction. |
| Alert on `absent(llz_credential_age_days)` instead of a probe gauge | Cannot distinguish a broken probe from a deployment that holds none of these credentials. The writer knows; ask it. |
| Filter the root token out of the presence rules rather than labelling `expect` | Leaves the highest-privilege credential in the platform unwatched, in the one state where it is dangerous. |
| One `atRestAllowed` entry for the shared phase-1 fallback | The first migrated root would vouch for the three that were not. Retiring it is per-root work. |
| Make `token-inventory` FAIL when its probe cannot authenticate | ADR 0009's fail-soft is right — an instance with no GitHub token should still get its Linode and PAT entries. The defect was invisibility, not softness, so the fix is a published verdict, not a hard exit. |
| Flag any scrape endpoint without `scheme: https` | An endpoint can legitimately carry no scheme where relabeling rewrites `__scheme__`. The registry is where that gets recorded — but the finding has to exist before it can be recorded. |
| A ValidatingAdmissionPolicy twin for the at-rest rules | Same objection ADR 0010 records for `plaintext-guard`: it would apply to apl-core's resources too and fail them closed at admission. These gates are scoped to what this repo ships, which is what this repo can fix. |
| Leave findings 5 and 6 alone because the tree is clean | "Latent" is what a drift gate is for. Both surfaces are ones a routine change touches — a new ServiceMonitor, a new Terraform root — and both are absent-by-default, so the regression is a line nobody wrote rather than one somebody has to defend. |

## Related

- ADR 0009 — the write-time mechanism this extends, and the source of findings 1–3.
- ADR 0007 — the state-encryption posture whose phase 2 is now registered with an exit test.
- ADR 0010 / 0011 — the in-cluster TLS posture and the ambient migration that closes most of `plaintextAllowed`.
- ADR 0001 — "where does the credential live" as a blast-radius question, which is why the OpenBao escrow cannot simply be moved into OpenBao.
