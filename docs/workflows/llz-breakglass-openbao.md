# `llz-breakglass-openbao` — maintainer rationale

`instance-template/.github/workflows/llz-breakglass-openbao.yml` is the reusable
(`workflow_call`) OpenBao break-glass root-token tool. It is **vendored verbatim
into every customer instance by copier**, alongside the composite actions it calls,
so the job runs self-contained with no cross-repo checkout. An instance ships a
~65-line caller stub (`breakglass-openbao.yml`) that owns the `workflow_dispatch`
trigger surface and vendors this body. The whole break-glass flow — validate,
revoke/rotate/regenerate, and RSA-OAEP-encrypt — is **one unit-tested Go verb**,
`llz ci bao-breakglass` (`tools/cmd/llz/ci_bao_breakglass.go`, itself layered on
`llz ci bao-regen-root`); it and `llz ci bao-status` are baked into `vars.TF_IMAGE`,
so the reusable is just cluster-access + that one verb + artifact upload. See
`docs/adr/0003-vendor-actions-and-bodies-into-instances.md` for the
surface-reduction pattern, and `template-scripts/AGENTS.md` for the
logic-belongs-in-tested-Go principle the `.untestable-budget.yaml` gate enforces.

Because the YAML is copied into instances where it can never be updated in place,
long-form maintainer archaeology lives here in the template repo; the inline
comments are the 3am debugging aids.

---

## Why this workflow exists

A root token is deliberately ephemeral: `bootstrap-openbao` (via
`llz-bootstrap-openbao.yml`) mints one, uses it to configure, and **revokes it**
unconditionally at the end for credential hygiene. Nothing durable holds a root
token. What survives is the **3-of-5 recovery quorum**
(`OPENBAO_RECOVERY_KEY_1/2/3`, held as `infra-<region>` environment secrets), which
authorizes `operator generate-root`.

Before this workflow, an operator who needed a root token during an incident had to
run the raw break-glass verbs by hand against a kubeconfig they fetched themselves
(see the "Break-glass handles" section of `docs/runbooks/bootstrap-openbao.md`).
That is fine as a last resort but has sharp edges: getting cluster access + the ACL
open, feeding the quorum, and — most dangerously — moving a live root token around
by hand. This workflow packages the safe path.

## ⚠️ Required prerequisite: protect the `infra-<region>` environments

This workflow converts **"can dispatch this workflow and pass the `infra-<region>`
environment gate"** into **"receives a full-admin OpenBao root token, encrypted to a
public key the dispatcher themselves supplies."** The encryption defends against
*log readers* — NOT against the dispatcher. Before this workflow existed, no CI path
ever handed a root token to a human (bootstrap minted, used, and revoked it in-job);
this one does, deliberately.

So the hard requirement: **every `infra-*` GitHub Environment MUST have protection
rules — at minimum required reviewers** (Settings → Environments → `infra-<region>` →
Required reviewers). Without them, any identity that can trigger a `workflow_dispatch`
(a leaked write-scope PAT, a compromised account) can mint cluster root with a clean,
quiet exfiltration path. The job summary records the dispatching `github.actor` next
to the ciphertext so delivery and audit trail live in one place — but that is a
*record*, not a *control*; the environment reviewers are the control.

## The three actions

One verb (`llz ci bao-breakglass --action …`), branched on `--action`:

- **generate** — `llz ci bao-regen-root`, then encrypt-and-deliver. `bao-regen-root`
  validates the loaded `OPENBAO_ROOT_TOKEN`; on a normal cluster that stored value is
  the **revoked** token from the last bootstrap, so it regenerates via the quorum.
  If it were somehow still valid, `bao-regen-root` skips regeneration and we deliver
  the existing token — still correct.
- **rotate** — revoke the current token *first*, then `bao-regen-root`. The ordering
  is load-bearing: `bao-regen-root` refuses to regenerate on an *inconclusive* token
  lookup precisely so a flaky exec never mints a second, untracked root while the
  first stays live. Revoking first makes the subsequent lookup a definite "revoked",
  so the regenerate branch is taken cleanly.
  **Mid-incident, prefer `generate`.** `rotate` is the riskier verb: because it
  revokes before regenerating, a regen failure *after* the revoke (a wrong quorum
  key, an exec flake) leaves you with **no live root token** until a later successful
  `generate`. That's the correct trade-off — better than leaking an untracked live
  root — but `generate` (which never revokes) is the safe default when you just need
  a token in hand; use `rotate` only when deliberately cycling a possibly-exposed one.
- **revoke** — `token revoke -self` against the stored token, then
  `gh secret delete OPENBAO_ROOT_TOKEN --env infra-<region>`. This is the cleanup
  half; root tokens have **no TTL**, so nothing expires them on its own.

## Why the token is encrypted, not printed

A root token is full admin. GitHub Actions run logs (and job summaries) are readable
by anyone with Actions access to the instance repo, and `::add-mask::` only filters
console output — it does not make a summary safe to share. So the operator supplies an
RSA **public** key (`recipient_pubkey_b64`, base64 of the PEM) and the token is
returned RSA-OAEP/SHA-256 encrypted; only ciphertext ever leaves the job (a base64
`root-token.b64` artifact + a job-summary block). The plaintext is also written to
`infra-<region>::OPENBAO_ROOT_TOKEN` by `bao-regen-root` (write-only in the UI), which
the `revoke` action later deletes.

### Why RSA-OAEP, and why the crypto is in Go (not openssl)

Encryption runs in `llz ci bao-breakglass` via Go's `crypto/rsa.EncryptOAEP` (SHA-256
for both the label hash and MGF1) — so the parsing, the private-key/too-small-key
rejections, and the round-trip are **unit-tested** (`ci_bao_breakglass_test.go`)
rather than an untested inline `openssl pkeyutl` heredoc that would spend
`.untestable-budget.yaml`. The **decrypt** recipe printed to the summary is plain
`openssl pkeyutl -decrypt -pkeyopt rsa_oaep_md:sha256`, guaranteed present on the
operator's machine (openssl does TLS) — no `age`/`gpg` dependency at the moment you
need it. The token is short (< 96 bytes), well under the RSA-OAEP single-block limit
for a ≥ 2048-bit key, so there is no hybrid-encryption envelope to manage. If a future
maintainer wants SSH-key recipients (reusing operators' existing `~/.ssh` keys), age
would be the natural switch — at the cost of shipping/pinning the `age` binary.

### Why one process, not one step per action

The verb does the whole flow in a single process, which is not just tidiness: it reads
the effective root token straight from its own `os.Getenv` immediately after
regeneration. The earlier step-per-action shape had to hand the regenerated token
between steps through `$GITHUB_ENV` — and a static job/step-level `env:` binding of
`OPENBAO_ROOT_TOKEN` is re-applied over that hand-off for every later step, silently
**shadowing** the regenerated value so the encrypt step would deliver the *stale*
(usually revoked) stored token. In one process there is no hand-off to shadow.

## Concurrency: shares the bootstrap group on purpose

The reusable sets `concurrency: openbao-bootstrap-<region>` — the **same** group as
`llz-bootstrap-openbao.yml` (and `terraform.yml`'s chained bootstrap job). Both drive
the `operator generate-root` quorum on `platform-openbao-0`, and two of those racing
would corrupt each other's nonce. Sharing the group serializes break-glass against
real bootstraps of the same deployment.

This does **not** reintroduce the caller/reusable deadlock that
`bootstrap-openbao.yml`'s header warns about: that deadlock happens only when a
*top-level* workflow shares a group with the `call` job it invokes. The caller stub
here sets no concurrency; only this reusable does, and it calls no further reusable.

## Inputs and secrets

### `instance_repo`

Passed from the caller (`<@ instance_repo @>`) and used as `GH_REPO` so `gh` targets
the repo directly instead of shelling to git — which fails on "dubious ownership" in
the container's checkout dir. `bao-regen-root`'s env-secret write and the `revoke`
delete both need it.

### `template-ref`

The template release the instance is rendered from — `llz upgrade` re-pins it. It is
**unused by this workflow's jobs** (everything resolves locally, from the vendored
copy). It is declared only because the caller stub passes it and `workflow_call`
rejects undeclared inputs.

### `action` typed as string

`workflow_call` has no `choice` input type, so the reusable declares `action` as
`string` and `llz ci bao-breakglass` re-checks it (rejecting anything but
generate/rotate/revoke). The caller's `workflow_dispatch` input IS a `choice`, so the
operator-facing surface is still a dropdown.

### Secrets are all `required: false`

Same rule as `llz-bootstrap-openbao.yml`: `secrets: inherit` cannot forward
environment-scoped secrets, and `required: true` is checked statically at the call
boundary — it would fail the run before the job enters `environment: infra-<region>`.
Presence is enforced at runtime instead (the recovery keys by `bao-regen-root`, the
write PAT by the `gh` calls).

## Prerequisites (met on any bootstrapped cluster)

- The cluster is up and **auto-unsealed** — `generate-root` requires an unsealed
  leader. `bao-status` runs first and reports the seal state; `bao-regen-root` and
  `token revoke` both hard-fail with a clear message if pod-0 is sealed. A sealed
  cluster is a seal-key problem, not a break-glass one — see the "Re-seal recovery"
  section of the runbook.
- `infra-<region>` secrets: `OPENBAO_RECOVERY_KEY_1/2/3`,
  `OPENBAO_SECRETS_WRITE_TOKEN` (writes/deletes the `OPENBAO_ROOT_TOKEN` env secret —
  the PAT must be Environments admin on the `infra-<region>` Environment),
  `TF_STATE_ACCESS_KEY` / `TF_STATE_SECRET_KEY`, and `LINODE_API_TOKEN`.
- **No** `OPENBAO_SEAL_KEY` is needed — that is only for a namespace/data rebuild.
