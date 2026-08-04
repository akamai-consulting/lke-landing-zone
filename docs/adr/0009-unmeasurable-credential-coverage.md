# ADR 0009 — Credential-age coverage for credentials with no measurable expiry

Status: **accepted**, implemented. Supersedes this ADR's own first draft, which
proposed self-asserted `rotation-stamps`; observation turned out to be available
for every credential that mattered, and the residue is named below.

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
| `TF_STATE_ENCRYPTION_PASSPHRASE` | The decryption of that state (ADR 0007 (state encryption)) | A passphrase. There is nothing to probe — no issuer, no expiry, no remote object. |
| LKE admin kubeconfig | Cluster-admin on every cluster | Rotated monthly by `secret-rotation.yml` scope `lke-admin`, but the artifact is a kubeconfig, not a token with an expiry claim we read. |

For Kind 2 the only meaningful signal is **age**: *when was this last rotated?*
And age, in this platform, is `updated_time` on an OpenBao KV path. That is the
whole mechanism — `llz_credential_age_days` is a thin wrapper over KV metadata.

So "measure these" reduces to "put these in OpenBao", and that is where it stops
being a plumbing change.

### Why "just put them in OpenBao" does not work

Two of the three are **bootstrap-ordering circular**, the same shape ADR 0007 (state encryption) hit
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

Two mechanisms, neither of which stores a credential anywhere new.

### 1. Measure write time, not age-in-a-vault

`GET /repos/{owner}/{repo}/environments/infra-<region>/secrets/<name>` returns
`{name, created_at, updated_at}`. Age is `now - updated_at`.

This is an **observation**, not an assertion, and it needs no storage — so the
circularity above simply does not arise. The security property is also stronger
than the OpenBao metadata reads: GitHub Actions secrets are **write-only over the
API**. No endpoint returns a value at any permission level, so a metadata probe
cannot leak the credential even by accident. With OpenBao we rely on a data grant
being absent; here the capability does not exist.

The obvious objection — `updated_at` measures the secret *object*, so re-writing
an unchanged value reports a rotation that never happened — was checked against
the actual writers rather than assumed:

| Writer | Behaviour |
|---|---|
| `llz tokens` | Write-on-missing (`if !have(...)`). Skips already-set secrets. |
| `secret-rotation.yml` (`tf-state-key`, `state-passphrase`) | Writes only on a real rotation. |
| A human re-pasting in the UI | The one false-refresh path — rare, and "someone deliberately touched this secret" is a defensible reset. |

An **absent** secret is reported `unknown` rather than dropped. That is the half
the rejected stamp design got wrong: the OpenBao sampler treats 404 as "not seeded
yet" and skips it, so a never-written credential is indistinguishable from a
healthy one. The API distinguishes them, so no companion absence alert is needed.

Runs inside the existing `token-inventory` CI job, which already holds a GitHub
token with Secrets:write (⊇ metadata read). No new credential, job, or schedule.
Published as `llz_credential_age_days` — the *same* metric the OpenBao sampler
uses, because it means the same thing and only the source differs, so the existing
dashboard panels and alert rules pick these up with no query changes.

### 2. Give the passphrase a real rotation path

The passphrase was classed "static by design" because rotating it means
re-encrypting every state file. Three facts, each **verified against OpenTofu
1.12.3** rather than reasoned about, make that rotation routine:

1. **`enforced` bans `method.unencrypted`, not `fallback`.** The roots'
   `encryption.tf` says the two are "mutually exclusive"; that is true only of the
   *unencrypted* fallback. An **encrypted** fallback is legal alongside
   `enforced = true`, so a rollover never relaxes the posture. (This ADR's earlier
   draft assumed a rotation window would require dropping `enforced` — it does not.)

2. **The key-provider NAME is decryption metadata.** pbkdf2 stores its salt at
   `meta["key_provider.pbkdf2.<name>"]`, so a passphrase decrypts only state
   written under the *same* name. Redefining `llz` to hold a new passphrase feeds
   the old salt to the new key: `decryption failed for all attempted`. Rotation
   therefore introduces a **new name** (`TF_STATE_ENCRYPTION_KEY_NAME`) and keeps
   the old one for the fallback (`…_KEY_NAME_OLD`). This is the non-obvious
   constraint and the reason the key name is now a tracked repo variable.

3. **`tofu state pull | tofu state push -` re-encrypts with no provider API
   calls**, and the plaintext lives only in the pipe between the two processes.
   Cheaper than `apply -refresh-only` and immune to a provider outage.

Because ADR 0007 (state encryption) put the *posture* in code and the *key material* in
`TF_ENCRYPTION`, the second key provider and the fallback are emitted entirely
from the `terraform-init` action — **no root is edited, and no PR is open during
the rotation window.**

The rollover is `secret-rotation.yml` scope `state-passphrase`: dispatch-only,
confirmation-gated, absent from both the schedule and `all`. Per root it re-keys,
then reads the state back **with the new key alone**; `llz ci
rotate-state-passphrase` exits non-zero unless every root verifies, and deleting
`TF_STATE_ENCRYPTION_PASSPHRASE_OLD` is gated on that. Until then the old
passphrase is the only thing that can read a straggler, so it is never discarded
early. Re-running converges, which is the recovery path.

With a real rotation path, the passphrase is `on-demand` (90-day SLA), not
`static` — the taxonomy's own test is whether the age is actionable.

### What this does not cover

The **LKE admin kubeconfig** is not a GitHub secret and has no `lke_*` event for
regeneration. It stays unmeasured, and is the honest residue of this ADR.

`rotation-stamps` — a metadata-only OpenBao path holding self-asserted timestamps
— is **withdrawn**. Both credentials it was meant to serve are now measured by
observation, and an assertion by the rotating job was always the weaker signal.

## Consequences

- The dashboard's "no rotation automation" panel stops being a half-truth: it
  implied the listed set was *all* the un-rotated credentials, when three of the
  highest-value ones were not in the query at all.
- The state-backend credentials get a rotation SLA for the first time.
- **The state-encryption passphrase becomes rotatable**, which it was not. That
  removes the "escrow it forever, it can never change" property — a compromised
  or suspected-compromised passphrase now has a remedy other than rebuilding.
- **A rollover is all-or-nothing across an instance.** A half-completed one
  leaves roots split across two keys. Mitigated by retaining the old passphrase
  until every root verifies, by re-runs converging, and by an exclusive
  concurrency group — but it is the sharp edge of this decision and belongs in
  the runbook, not a footnote.
- **`TF_STATE_ENCRYPTION_KEY_NAME` is now load-bearing config.** If the variable
  drifts from the name state was actually written under, decryption fails —
  loudly, not silently, which is the right failure, but it is a new way to break
  a working instance.
- `tofu state pull` emits plaintext state. Every path that runs it keeps stdout
  out of buffers, files, and logs; only stderr is captured for errors.
- No new credential material is stored anywhere. The measurement half is pure
  observation, and the rollover moves key material between an Actions secret and
  `TF_ENCRYPTION` — never into a file, a log, or OpenBao.
- One more thing that fails soft: an instance with no GitHub token still gets its
  Linode and PAT entries, and a probe error reports `unknown` rather than taking
  the inventory down.

## Alternatives rejected

| Option | Why not |
|--------|---------|
| `rotation-stamps` (this ADR's first draft) | A timestamp asserted by the rotating job, not an observation: a job that stamps but fails to publish the new credential reports success. Superseded — observation was available after all. |
| Store the credentials themselves in OpenBao | Circular: OpenBao runs inside the cluster whose state they guard. Also widens blast radius — the single pane's job is to *observe* credentials, not to hold them. |
| Probe the Linode Object Storage keys API for `created` | **The field does not exist.** `ObjectStorageKey` is `{id, label, access_key, secret_key, limited, bucket_access, regions}` (verified, linodego v1.65.0). The repo's own rotator orders keys by monotonic `id` for exactly this reason. |
| Derive OBJ-key age from the `obj_access_key_create` account event | Real, and carries a true `Created` — but bounded by event retention (unverified, and if shorter than the SLA every key reads as overdue), needs `account:read_only`, and covers neither the passphrase nor the kubeconfig. `updated_at` is simpler and covers all three. |
| `apply -refresh-only` to re-key | Works, but calls provider APIs for every root × deployment, so a provider hiccup fails a rollover that has nothing to do with the provider. `state pull \| push` does the same re-encryption with no API calls. |
| Reuse the key-provider name across a rotation | Verified to fail: the old salt is fed to the new passphrase. This is why the key name is versioned rather than fixed. |
| Leave them unmeasured, document the gap | Honest, and cheaper. Rejected because the gap is not uniform: these are the state bucket and its decryption key, i.e. the credentials whose compromise is least recoverable. |

## Related

- ADR 0007 (Terraform state encryption) — same circularity, same rejection of
  `key_provider "openbao"`; this ADR generalises that reasoning from *keys* to
  *observability of keys*.
- The **PAT-rotation-locus** question — "where does the credential live" as a blast-radius question. (Reserved as ADR 0001; not yet written — see [the ADR index](README.md).)
- [docs/secrets.md](../secrets.md) — the rotation-class table and the
  credential-age coverage section this extends.
- `tools/cmd/llz/ci_token_inventory.go` — Kind 1's target list.
