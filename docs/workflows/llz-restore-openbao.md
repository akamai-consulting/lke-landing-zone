# `llz-restore-openbao` — maintainer rationale

`instance-template/.github/workflows/llz-restore-openbao.yml` is the reusable
(`workflow_call`) workflow that restores a deployment's OpenBao from one of the Raft
snapshots the `openbaoBackup` CronJob takes. It is **vendored verbatim into every
instance by copier** (class `managed`, digest-locked); an instance ships a thin,
token-free caller stub (`restore-openbao.yml`, class `merge`) that owns the
`workflow_dispatch` surface. The logic is two unit-tested Go verbs —
`llz ci openbao-restore` and `llz ci bao-revoke-root`
(`tools/internal/extensions/lifecycle/openbao/ci_openbao_restore.go`,
`ci_openbao_restore_root.go`) — so the YAML is a request check, cluster access, one
verb per mode, bootstrap's configure step, and two `always()` revokes.

The operator procedure is the runbook,
[`docs/runbooks/openbao-backup-restore.md`](../runbooks/openbao-backup-restore.md).
This page is the why.

---

## Why this workflow exists

`llz openbao restore` needs a root token on the cluster it restores into, and a
workstation can mint one only from recovery shares the operator holds
(`llz openbao regen-root`). A deployment bootstrapped **without** an escrow key holds
its shares only as `infra-<deployment>` environment secrets, which are write-only:
nothing outside a workflow job in that environment can read them. Before this
workflow, such an operator's only route to a restore was break-glass, which writes the
token back into the environment — where, again, only a workflow can read it. #571
named the gap (its "Decisions for review" item 5) and this closes it: the same
restore, run inside a job in `infra-<deployment>`, with root minted, used and revoked
in that job.

## The job, step by step

### Check the request (mode, snapshot, typed confirmation, spec)

`llz ci openbao-restore --preflight`. First, unconditional, and before anything is
opened: `mode` is `list | plan | restore`; `snapshot` XOR `latest` for plan/restore
(no default — after an incident the newest snapshot may already hold the damage);
`from_cluster` only with `latest`; and for `restore`, `confirm` must equal
`restore:<deployment>` exactly — the token `llz openbao restore --confirm` takes,
and the same typed-confirmation pattern as `llz ci assert-destroy-confirm`. A
mistyped dispatch therefore opens no control-plane ACL and spends no quorum.

It also **refuses plan/restore on a deployment whose spec disables
`components.openbaoBackup`.** With the component off nothing is producing snapshots,
and a bucket it once filled is stale by a margin the dispatch form does not show;
the workflow would roll the store back to whenever backups stopped. The one
legitimate case is a rebuild, where the runbook used to keep the component off until
after the restore so the empty rebuilt store does not back itself up. The runbook's
workflow path turns it back on first (harmless: retention is per OpenBao cluster id,
so the rebuilt cluster's empty snapshots can never prune the lost cluster's, and
`latest` refuses to choose between the two); the workstation path does not refuse.
`list` stays allowed — reading the bucket is how an operator finds out what is left.

### List the snapshots

`mode=list` is `llz openbao backups`, and runs **without cluster access**: the bucket
is located from the spec and read with a temporary read-only key minted from
`LINODE_API_TOKEN`, so it works with the cluster gone — which is when it matters.

### Plan the restore (dry run)

`mode=plan` is `llz openbao restore` without `--yes`: fetch and verify the snapshot,
prove the kube-context is this deployment's LKE cluster (the check is the restore
verb's own and runs here too), find the active pod, read the live cluster id, and say
`match` or `MISMATCH`. Nothing changes.

### Restore OpenBao from the snapshot

`mode=restore`. The verb runs `llz openbao restore`'s two halves with what a
workstation does by hand around them:

1. **prepare** — everything the dry run does. Changes nothing.
2. **ask Apply's guards first** (`baosnap.CheckApplicable`): a foreign snapshot
   without `force`, a sealed or standby target. Every refusal the restore can reach
   lands **before** a quorum is spent on a token for it.
3. **mint root #1** from `OPENBAO_RECOVERY_KEY_1..3` through the quorum code
   `bao-regen-root` uses (`generateRootPod` + `generateRootQuorum`, shared — so the
   standby, sealed and break-on-complete lessons apply here too). The token is
   masked and written to `$RUNNER_TEMP/openbao-restore-root.token` (0600) **before**
   it is verified, so the revoke step can find it however the verb exits.
4. **record** every tracked credential path (`credpaths.CredPaths`).
5. **apply** — re-prove the kube-context immediately before the irreversible call,
   restore, wait for OpenBao to come back unsealed.
6. **mint root #2** on the restored store. A Raft restore replaces the token table,
   so root #1 — minted after the snapshot — no longer exists; `revokeRootToken`
   proves that (OpenBao answers "rejected") rather than assuming it, and refuses to
   mint over a token whose state it could not determine.
7. **re-apply credentials** the restore reverted (`baosnap.ReapplyCreds`).
8. **check the stored root token**: is `infra-<deployment>::OPENBAO_ROOT_TOKEN` live
   in the restored store?
9. **write the job summary**: what ran, and the checklist of what did not.

### Re-apply the declared OpenBao configuration

`llz ci bao-configure` — bootstrap's own configure step — under root #2, only when
the restore step fully succeeded. It is a separate step rather than a call from the
restore verb because it belongs to another extension (identity-plane), and an
extension-to-extension import is the coupling the split exists to prevent.

### Revoke the root token this run minted / Revoke control-plane ACL

Both `if: always()`, in that order: the token revoke needs the cluster, so it runs
before the ACL closes. `llz ci bao-revoke-root` reads the token file, revokes the
token or proves it already gone, and deletes the file; no file means nothing was
minted. It fails when the answer is inconclusive — root tokens do not expire — and
names the accessor the mint step printed.

## Why the token never reaches GitHub

`bao-regen-root`, the verb bootstrap and break-glass use, exports the token to
`$GITHUB_ENV` and writes it back to `infra-<deployment>::OPENBAO_ROOT_TOKEN`: the
bootstrap job hands it to a dozen later steps, and the next bootstrap reuses it. A
restore needs neither, and a token parked in GitHub outlives the run, trips
`LLZCredentialRootTokenParked`, and is one more copy to revoke. So the persistence
half is not shared: the token lives in the verb's process and in one 0600 file under
`$RUNNER_TEMP`, which is per-job and wiped when the job ends. The configure step
reads it from that file for its own process only — not from `$GITHUB_ENV`, which
would put it in the environment of every later step, including the third-party
actions; and the `$GITHUB_ENV` shadowing trap described in
[llz-breakglass-openbao.md](llz-breakglass-openbao.md#why-one-process-not-one-step-per-action)
does not arise. Structurally, the workflow is **not handed `OPENBAO_SECRETS_WRITE_TOKEN`
or a `GH_TOKEN`**, so it cannot write a secret back even by mistake;
`TestRestoreWorkflowCannotWriteATokenBack` pins that.

`OPENBAO_ROOT_TOKEN` *is* read — only to ask the restored store whether the snapshot
brought it back to life.

## Which post-restore steps are automated, and which are not

The deciding question for each was: can doing it automatically make things worse?

| Step | Automated? | Why |
|---|---|---|
| Re-apply tracked credentials the restore reverted | **Yes** | The value live just before the restore is the one the outside world still accepts — the rotators wrote it and revoked its predecessor; the snapshot's is often a revoked key. Same rule and same code (`baosnap.ReapplyCreds`) as the e2e drill. The one way it can be wrong — the live value was itself the damage — is why every re-applied path is named in the summary. After a rebuild the live values are the rebuild's freshly minted keys, which are equally current. |
| Re-apply the declared configuration (`bao-configure`) | **Yes, after every restore** | The restored policies, roles and auth mounts are the snapshot's: an older template's, and after a rebuild the team-login mount points at the lost cluster's domain. Kubernetes auth discovers its CA and reviewer JWT in-pod, so the stored config names no cluster, but re-applying it costs nothing. It is bootstrap's configure step — idempotent upserts converging on the declared state — and it already runs on every bootstrap, so it is no new kind of write. #571's runbook asked for it after a rebuild; running it after every restore also catches a snapshot older than the last template upgrade. |
| Revoke `infra-<d>::OPENBAO_ROOT_TOKEN` if the restore revived it | No — reported | Someone may be holding it mid-incident. Break-glass `action=revoke` is one dispatch. |
| Re-apply operator writes since the snapshot | No | Only the operator knows which were intended. |
| Rotate the database admin credential | No | `secret/infra/db-admin/*` is not a tracked path, and the rotation resets the password in place with no overlap — an outage nobody asked for. |
| Take a backup of the restored store | No | It counts against the newest `backupKeep` of that cluster id — after a rebuild, the lost cluster's history — and pushes the oldest restore point out. The next scheduled tick does the same on its own clock, and the backup alerts cover a failure. |
| Reconcile an HA peer | No | The drift check reads both halves and decides which value is newer: a judgement. |

## When the post-restore steps cannot run

After a restore the store answers only to the recovery shares it had when the
snapshot was taken. After a **rebuild** the rebuild's `bao-init` replaced the shares
in `infra-<deployment>`, so root #1 (on the rebuilt, empty store) mints fine and root
#2 (on the restored store) cannot. The restore stands; the step fails; the summary
says exactly why, lists the credential paths that were not re-applied, and sends the
operator to put the snapshot-era shares back and re-run `bootstrap-openbao.yml`. One
set of environment secrets cannot open both stores, and taking a second set of shares
as dispatch inputs would put recovery material on a form — so this is stated, not
worked around.

## Concurrency: the bootstrap group, on the body only

`concurrency: openbao-bootstrap-<region>`, the same group as
`llz-bootstrap-openbao.yml` and `llz-breakglass-openbao.yml`. All three drive
`operator generate-root` on the active pod and would corrupt each other's nonce; and a
bootstrap configuring the store while it is replaced underneath it would write into a
store about to vanish. The stub sets none — a caller sharing its reusable's group
deadlocks (see the note in `bootstrap-openbao.yml`).
`TestRestoreWorkflowSharesTheBootstrapGroup` pins both halves.

## Inputs and secrets

- `mode` is a `choice` on the stub defaulting to **`plan`**, and a `string` on the
  body (`workflow_call` has no choice type), re-checked by the verb. A dispatch that
  changes nothing on the form is a dry run.
- Every input reaches a `run:` through the job's `env:`, never as `${{ }}` in the
  script (the workflow-injection guard; `TestRestoreWorkflowRoutesInputsThroughEnv`).
- Secrets are all `required: false`, for the same reason as the bootstrap body:
  `secrets: inherit` cannot forward environment-scoped secrets, and `required: true`
  is checked statically at the call boundary. The recovery keys are mapped into the
  restore step only.

## What exercises it

- **Release e2e dispatches it in `list` mode** after provision
  (`release-e2e-lane.yml`, "Restore workflow (list) reaches the backups"): the stub
  starts, the body enters `infra-e2e`, and the bucket read finds the snapshots the
  drill just produced (the verb fails on an empty bucket). `plan` is not driven: the
  e2e bucket outlives its clusters and holds several OpenBao cluster ids, so `latest`
  rightly refuses and the lane cannot name this cluster's id. `restore` is not driven:
  the in-place restore is the drill's (`assert-openbao-restore`, the same `baosnap`
  functions), and a second in-place restore per release would add a quorum mint for
  little extra signal.
- **The restore mode's own steps** — mint without write-back, refusals before the
  quorum, credential re-apply, the rebuild failure, the revived stored token, the
  revoke — are unit-tested against a fake OpenBao whose restore rolls back its KV
  state **and its token table** (`ci_openbao_restore_test.go`). A fake that kept its
  tokens would let "revoke after the restore" pass by revoking a token the real store
  no longer has.
- **The step order** — request check first, revoke `always()` after the restore and
  configure and before the ACL closes, no token write-back, the shared group, the
  `plan` default — is `ci_openbao_restore_workflow_test.go`, over the parsed YAML.

What none of that proves is a `mode=restore` dispatch against a real cluster; see the
PR that added it.
