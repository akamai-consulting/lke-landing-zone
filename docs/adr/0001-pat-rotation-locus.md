# ADR 0001 — Locus of Linode PAT rotation: broad in-cluster, narrow stays in CI

- Status: Accepted
- Date: 2026-07-15
- Deciders: platform / LLZ maintainers
- Related: `docs/designs/linode-pat-dns-consolidation.md`,
  `docs/designs/linode-credential-rotator.md`,
  `docs/designs/instance-slimming.md`,
  `platform-apl/components/broadPatRotator/`

## Context

Two Linode Personal Access Tokens back the platform:

1. **Broad PAT** — `account:read_write` (label `gha-platform-platform_LINODE_API_TOKEN`,
   surfaced as the `LINODE_API_TOKEN` env secret). Every CI workflow and Terraform
   run reads it; it is the one token that can *mint* other PATs. Account-wide: a
   single token family serves all deployments.
2. **Narrow in-cluster PAT** — the scoped token (`domains:read_write`,
   `object_storage:read_write`, `volumes:read_write`, `linodes:read_only`,
   `vpc:read_only`, `firewall:read_write`; label `llz-incluster-<region>`, at
   `secret/linode/api-token`) that in-cluster consumers read. Deliberately withholds
   `account:read_write`, so it **cannot self-mint** a replacement.

Historically both rotated in GitHub Actions (`llz-secret-rotation.yml`): a
`create-linode-pat` job minted the broad PAT and republished it to each
deployment's env secret, a daily `revoke-linode-pat` job drained old broad
siblings, and a per-region `propagate-linode-pat` job minted each region's narrow
PAT *using the broad token* and wrote it to that region's OpenBao.

The instance-slimming effort (`docs/designs/instance-slimming.md`) moved standing
platform work out of the per-instance GitHub-Actions surface and into in-cluster
CronJobs. The broad PAT's create/revoke was a natural candidate: it is a pure
Linode-API + GitHub-env-secret-write operation with no Terraform-state or
hosted-runner dependency. The `broadPatRotator` CronJob
(`platform-apl/components/broadPatRotator/`) now owns it — mint → verify →
write to OpenBao → publish to each deployment's env secret → revoke old siblings —
holding the broad token via ESO on **exactly one** deployment (it is account-wide,
so more than one owner would race mint/revoke).

That raised the obvious follow-on question: should the **narrow** per-region PAT
re-mint move in-cluster too, retiring `propagate-linode-pat` from CI?

## Decision

**The broad PAT rotates in-cluster (`broadPatRotator`). The narrow in-cluster PAT
re-mint stays in CI (`propagate-linode-pat` in `llz-secret-rotation.yml`).**

The narrow PAT lacks `account:read_write` and cannot self-mint; minting its
replacement requires the broad `account:read_write` token. Moving the narrow
re-mint in-cluster would therefore require the broad token to be present, via ESO,
in **every** cluster's `llz-pat-rotator` namespace — expanding the account-wide
token's blast radius from one cluster to all of them. Alternatives considered and
rejected:

- **Broad rotator mints every region's narrow PAT and publishes each to that
  region's `infra-<d>` GitHub env secret.** Keeps the broad token on one cluster,
  but re-introduces the narrow token transiting a GitHub secret — the exact
  round-trip the current per-region OpenBao-write design removed.
- **Give every cluster the broad token.** Simplest, but directly negates the
  isolation `broadPatRotator` was designed to preserve, and every in-cluster
  workload compromise on any cluster would then expose an account-wide token.

Keeping the narrow re-mint in CI keeps the broad `account:read_write` token in
exactly two places — CI (env secret) and the single `broadPatRotator` cluster —
while the narrow token continues to be minted per-region and written straight to
each region's OpenBao, never crossing a job boundary or a GitHub secret.

To keep the narrow PAT rotating on schedule after `create-linode-pat` was deleted,
the monthly rotation leg now routes to `run-pat-propagate-only` (see
`llz ci rotation-plan` / `tools/internal/extensions/assertions/tokeninv/rotationplan.go`): the narrow re-mint
runs monthly against whatever broad token the env secret currently holds (kept
current by `broadPatRotator`'s env-secret publish), decoupled from — and no longer
gated on — an in-CI broad-PAT create.

## Consequences

- `llz-secret-rotation.yml` loses the `create-linode-pat` and `revoke-linode-pat`
  jobs, the `linode-pat` / `linode-pat-revoke` dispatch scopes, and the
  `pat-apply` / `revoke-apply` inputs. `propagate-linode-pat` no longer `needs`
  the deleted create job; it gates solely on `run-pat-propagate-only`.
- The monthly schedule routes `run-pat-propagate-only=true` (narrow re-mint) in
  place of the former `run-pat-create`; the daily schedule routes only the
  TF-state OBJ key reaper (broad PAT drain is now `broadPatRotator`'s job).
- `broadPatRotator` must be enabled on exactly one deployment; if it is disabled
  everywhere, the broad PAT stops rotating (the CI create job that used to be the
  backstop is gone). The credential single-pane (`llz ci token-inventory` →
  reconciler → `LLZToken*` alerts) surfaces an aging/expiring broad PAT.
- The broad `account:read_write` token's standing footprint is one CI env secret
  plus one cluster — unchanged from before this ADR, and explicitly *not* widened
  to every cluster.

## Revisiting

Reopen this decision if Linode confirms a scoped PAT may self-mint (design §5 of
`linode-pat-dns-consolidation.md` lists this as unconfirmed). If a narrow token
can mint its own successor, the narrow re-mint could move in-cluster without the
broad token ever leaving CI + the single broad-rotator cluster.
