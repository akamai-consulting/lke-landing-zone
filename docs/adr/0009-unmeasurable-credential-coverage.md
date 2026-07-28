# ADR 0009 — Credential-age coverage for credentials with no measurable expiry

Status: **proposed**. Kind 1 (below) is implemented in the same PR and needs no
decision from this ADR — it is described here only to draw the boundary. Kind 2
is what this document is for.

## Context

The credential single pane answers one question: *is any credential in this
platform overdue for rotation?* It answers it from three feeds:

| Feed | Series | Source |
|------|--------|--------|
| Token inventory | `llz_token_expiry_timestamp_seconds`, `llz_token_audit_ok` | `llz ci token-inventory` — holds a token, reads its **expiry** |
| OpenBao age sampler | `llz_credential_age_days{cred,class}` | `--reconcile-openbao-gauges` — reads KV-v2 `updated_time` |
| cert-manager | `certmanager_certificate_expiration_timestamp_seconds` | scraped directly |

After the recent coverage work, **every OpenBao path a platform policy knows
about is age-tracked — 16 of 16.** The remaining blind spots are all credentials
that live *outside* OpenBao, in GitHub Actions secrets. They split into two kinds
that need different mechanisms:

**Kind 1 — has a readable expiry.** `E2E_DISPATCH_TOKEN`, `GHCR_READ_TOKEN`.
GitHub returns `github-authentication-token-expiration` on any authenticated
request, so `gatherGitHubTokens` already knew how to measure these. The only
obstacle was that the target list was two hardcoded literals at the call site.
That is a plumbing change, not a design question — now `ghPATTargets`, with the
two optional PATs dropped rather than reported `unknown` when unset.

**Kind 2 — has no expiry to read at all.** These are the subject of this ADR:

| Credential | What it guards | Why it cannot be measured today |
|------------|----------------|---------------------------------|
| `TF_STATE_ACCESS_KEY` / `TF_STATE_SECRET_KEY` | The state bucket — which, post-ADR-0007, holds *encrypted* state containing `kubeconfig_raw` and every `root_password` | Object Storage keys are not PATs. `/profile/tokens` does not return them, and the key resource carries no expiry field. |
| `TF_STATE_ENCRYPTION_PASSPHRASE` | The decryption of that state (ADR 0007) | A passphrase. There is nothing to probe — no issuer, no expiry, no remote object. |
| LKE admin kubeconfig | Cluster-admin on every cluster | Rotated monthly by `secret-rotation.yml` scope `lke-admin`, but the artifact is a kubeconfig, not a token with an expiry claim we read. |

For Kind 2 the only meaningful signal is **age**: *when was this last rotated?*
And age, in this platform, is `updated_time` on an OpenBao KV path. That is the
whole mechanism — `llz_credential_age_days` is a thin wrapper over KV metadata.

So "measure these" reduces to "put these in OpenBao", and that is where it stops
being a plumbing change.

### Why "just put them in OpenBao" does not work

Two of the three are **bootstrap-ordering circular**, the same shape ADR 0007 hit
when it rejected `key_provider "openbao"`:

```
TF_STATE_ACCESS_KEY  ──unlocks──>  state bucket
                                        │
                                        ├─ cluster root ──creates──> LKE cluster
                                        │                                 │
                                        │                            runs OpenBao
                                        │                                 │
                                        └─────────────── would store the key ◄┘
```

OpenBao runs inside the cluster whose state is guarded by the very key we would
be storing there. Lose the key and you cannot read the state that tells you how
to reach the OpenBao that holds the key. `TF_STATE_ENCRYPTION_PASSPHRASE` is
worse: it is the *only* thing standing between an attacker with the bucket and
plaintext cluster-admin credentials, and it is unrecoverable — losing it makes
state permanently unreadable.

This is not a reason to leave them unmeasured. It is a reason not to measure them
by moving them.

## Decision

**Do not move Kind 2 credentials into OpenBao. Track their rotation age in a
dedicated metadata-only KV path that holds no credential material.**

Introduce `secret/infra/rotation-stamps` — a single KV path whose *keys* are
credential names and whose *values* are RFC3339 timestamps:

```
secret/infra/rotation-stamps
  tf-state-key              = "2026-07-28T14:02:11Z"
  tf-state-passphrase       = "2026-05-02T09:31:40Z"
  lke-admin-kubeconfig      = "2026-07-01T03:17:00Z"
```

The credential itself never enters OpenBao. Only the assertion "this was rotated
at time T" does — which is exactly, and only, what the dashboard needs. The
circularity dissolves because losing OpenBao loses a *timestamp*, not the key.

Whoever rotates the credential stamps it. `secret-rotation.yml` already owns the
`tf-state-key` and `lke-admin` scopes, so the stamp is one write at the end of a
job that has just succeeded.

### Why a single path rather than one per credential

`credPaths` is an enumeration and each entry costs a policy grant
(`TestCredPathsAreGrantedInReconcilerPolicy` pins the pair). A single path means
one grant for the whole class, and adding a credential later is a one-line change
with no policy edit — which matters, because a path added to `credPaths` without
its grant 403s and takes the **entire sampler pass** down, seal gauge included.

It also means the sampler needs a new read shape: these are per-**key** stamps
inside one secret, not one `updated_time` per path. That is a real addition to
`sampleOpenBao` (a data read, where every other credential entry is metadata
only) and is the main implementation cost of this decision.

### Class: `on-demand`

`tf-state-key` and `lke-admin` have real rotation paths a human can dispatch;
`tf-state-passphrase` does not yet have one at all. All three are actionable by a
human, none by a schedule — which is the existing `on-demand` definition, and
puts them on the 90-day SLA rather than the yearly info nudge.

### An absent stamp is not "fresh"

The failure mode to design against is a stamp that is never written: a missing
key must not read as a healthy credential. The sampler skips absent paths (404 =
"not seeded yet"), which is correct for an opt-in credential but wrong here — a
never-stamped `tf-state-key` would simply publish no series and look identical to
a well-managed one.

So this needs a companion alert on **absence**, keyed off the instance's declared
credential set rather than off what happens to be in the ConfigMap:
`LLZRotationStampMissing`, info, for a credential the instance is known to use
that has no stamp. Without it this decision buys visibility that silently fails
open — the same defect the `class` label was introduced to fix.

## Consequences

- The dashboard's "no rotation automation" panel stops being a half-truth: it
  currently implies the listed set is *all* the un-rotated credentials, when
  three of the highest-value ones were never in the query at all.
- The state-backend credentials get a rotation SLA for the first time.
  [docs/secrets.md](../secrets.md) records `TF_STATE_ACCESS_KEY` as having no
  scheduled rotation; a 90-day alert makes that a decision someone renews rather
  than a default nobody revisits.
- **Stamps can lie.** A stamp is an assertion by the rotating job, not an
  observation of the credential. A job that writes the stamp but fails to publish
  the new key reports success. This is strictly weaker than Kind 1's measured
  expiry and must be documented as such — mitigated by stamping only *after* the
  verify step, never before.
- One more thing bootstrap must not depend on. The stamp write has to be
  best-effort: a failed stamp must not fail a successful key rotation.
- No new credential material anywhere. The path is metadata-only by construction,
  so the `platform-ci` read grant stays as narrow as it is today.

## Alternatives rejected

| Option | Why not |
|--------|---------|
| Store the credentials themselves in OpenBao | Circular for the state key and passphrase — OpenBao lives in the cluster whose state they guard. Also widens blast radius: the single-pane's job is to *observe* credentials, not to become a place they are kept. |
| Probe the Linode Object Storage keys API for `created` | Gives creation time, not rotation time, and only for OBJ keys — nothing for the passphrase or the kubeconfig. Solves a third of the problem with a mechanism that does not generalise. |
| Derive age from GitHub's secret `updated_at` | The GitHub API does expose `updated_at` on an Actions secret, and it needs no new storage. But it measures *the secret object*, not the credential: re-running `llz tokens` or any re-set of an unchanged value refreshes it, so it reports freshness that did not happen. Worth revisiting only if stamping proves too easy to skip. |
| A `rotated_at` convention inside each credential's own secret | Exactly what `linodeCredRotator` does for the OBJ keys, and the right pattern — but it requires the credential to be in OpenBao, which is the thing this ADR cannot do. |
| Leave them unmeasured, document the gap | Honest, and cheaper. Rejected because the gap is not uniform: these are the state bucket and cluster-admin, i.e. the credentials whose compromise is least recoverable. |

## Related

- ADR 0007 (Terraform state encryption) — same circularity, same rejection of
  `key_provider "openbao"`; this ADR generalises that reasoning from *keys* to
  *observability of keys*.
- ADR 0001 (PAT rotation locus) — "where does the credential live" as a blast-radius question.
- [docs/secrets.md](../secrets.md) — the rotation-class table and the
  credential-age coverage section this extends.
- `tools/cmd/llz/ci_token_inventory.go` — Kind 1's target list.
