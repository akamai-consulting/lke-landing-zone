# Design: in-cluster Linode PAT rotation (CronJob)

**Status:** Draft / design-stage (skeleton below is not yet wired into any env).
**Item:** kube-native cred-hardening #4 — move the Linode PAT rotation loop out of
CI and into the cluster.
**Relates to:** [secrets.md](../secrets.md), the `linode-volume-labeler` CronJob
(`apl-values/components/volumeLabeler/`), the `secret-propagator` OpenBao policy
(`tools/cmd/llz/ci_openbao_configure.go`), and cred-hardening #3 (GitHub App) —
see "The dual-trust-domain problem".

## Problem

`LINODE_API_TOKEN` is a long-lived Linode PAT (90-day policy) rotated **from CI**:

1. `secret-rotation.yml` (scheduled) runs `llz credentials pat create` — mints a
   new PAT via the Linode API (`internal/linode.CreateProfileToken`), writes it to
   the `infra-<env>` GitHub Environment secret, and later `revoke-old` drains the
   previous one (`DeleteProfileToken`, keep-newest-N by label).
2. `llz ci propagate-pat` then pushes the new token into each region's OpenBao at
   `secret/linode/api-token` via the GitHub-OIDC `secret-propagator` role.
3. In-cluster consumers (`linode-volume-labeler`, and any future Linode-API
   consumer) read `secret/linode/api-token` through ESO.

This works but keeps a credential-rotation loop in CI with two moving parts
(GitHub secret write + OIDC-mediated OpenBao write) and a long-lived token whose
only in-cluster consumer is a cosmetic volume labeler. The kube-native target is
a controller that mints/rotates the token **in the cluster** and writes it
straight to OpenBao — no CI job, no `propagate-pat`.

## Goals / non-goals

**Goals**
- Rotate the *in-cluster* Linode token on a schedule, in-cluster, with no CI step.
- Reuse the existing, tested rotation primitives in `internal/linode`.
- Least-privilege: the in-cluster token carries only the scopes in-cluster
  consumers need (today: Volumes RW for the labeler), narrower than the broad
  provisioning PAT.
- Verify-before-revoke so a bad mint can never lock the cluster out of the Linode
  API.

**Non-goals**
- Rotating the **provisioning** PAT that Terraform/CI use (see the dual-trust
  problem — that credential lives outside the cluster and stays as-is here).
- A controller-runtime Deployment. Rotation is periodic, not a continuous
  reconcile, so a CronJob (the `linode-volume-labeler` shape) is the right tool
  and far less machinery.

## The dual-trust-domain problem (the crux)

`LINODE_API_TOKEN` today serves **two trust domains**:

| Consumer | Where | Needs |
|---|---|---|
| Terraform / CI provisioning | out-of-cluster (GitHub Actions) | broad scopes (Linodes, VPCs, OBJ, Firewalls, LKE) + the `infra-<env>` GitHub secret |
| `linode-volume-labeler` (+ future in-cluster consumers) | in-cluster | narrow scopes (Volumes RW) + `secret/linode/api-token` in OpenBao |

A purely in-cluster controller can own the **OpenBao** copy trivially, but it
**cannot** update the `infra-<env>` GitHub secret without GitHub write auth — which
means a stored GitHub PAT or the GitHub App from cred-hardening #3. So a single
shared token forces a coupling to #3.

**Recommendation — split the credential.** Mint a **separate, in-cluster-scoped
Linode PAT** (label e.g. `llz-incluster-<env>`, scopes = Volumes RW only) that the
CronJob owns end-to-end and writes only to OpenBao. The broad **provisioning** PAT
stays exactly as it is today (CI-rotated, in the GitHub secret). This:
- decouples #4 from #3 entirely (no GitHub write needed),
- shrinks the blast radius of the in-cluster token to Volumes,
- lets the controller fully own its credential's lifecycle.

The alternative — one token, controller also writes the GitHub secret via a GitHub
App — is viable but only **after** #3 lands, and keeps the broad scope in-cluster.
Prefer the split.

## Proposed design

A per-env **CronJob** (modeled on `linode-volume-labeler`) that runs a new
`llz ci rotate-linode-pat` subcommand. Each run:

1. **Read** the current in-cluster token from the ESO-synced `linode-api-token`
   Secret (env `LINODE_TOKEN`).
2. **Mint** a new PAT with the configured label/scopes/expiry
   (`internal/linode.CreateProfileToken`) — only when the newest existing token of
   that label is within the rotation window (age ≥ `--rotate-after-days`), so the
   CronJob can run frequently but rotate rarely (idempotent, like the labeler).
3. **Verify** the new token works (a cheap authenticated probe, e.g.
   `GET /v4/profile`) BEFORE touching anything else.
4. **Write** the new token to OpenBao `secret/linode/api-token` using a
   Kubernetes-auth role bound to a write-scoped policy (mirror `secret-propagator`,
   below). ESO then syncs it to consumers within its `refreshInterval`.
5. **Drain** older tokens of the same label past a grace window
   (`DeleteProfileToken`, keep-newest-N) — never the one just written, never
   before the new one is confirmed in OpenBao.

Verify-before-revoke + keep-newest-N means a failed mint or a bad token leaves the
previous working token in place; the next run retries.

### Components (skeleton — `apl-values/components/linodePatRotator/`)

Mirror `volumeLabeler/linode-volume-labeler/`:

- **`namespace.yaml`** — `llz-linode-pat-rotator`, PSS-restricted.
- **`externalsecret.yaml`** — ESO syncs `secret/linode/api-token` → `linode-api-token`
  Secret (the *input*: the current token, used to mint the next).
- **`cronjob.yaml`** — schedule e.g. `17 3 * * *` (daily; rotates only when due),
  `concurrencyPolicy: Forbid`, runs `llz ci rotate-linode-pat`. SA below.
- **`rbac.yaml`** — ServiceAccount + minimal RBAC (TokenRequest for OpenBao
  k8s-auth login; no cluster API writes needed).
- **`network-policy.yaml`** — egress to the Linode API (443), OpenBao
  (`platform-openbao:8200`), DNS, and the K8s API (LKE-E 443→6443).
- **`kustomization.yaml`** — `kind: Component`, gated on `spec.components.linodePatRotator`.

### OpenBao auth (write path)

The CronJob's ServiceAccount logs in via Kubernetes auth to a role mapped to a
write-scoped policy. `secret-propagator` already grants exactly the needed paths:

```hcl
path "secret/data/linode/api-token" { capabilities = ["create", "update", "read"] }
path "secret/metadata/linode/api-token" { capabilities = ["read"] }
```

Add a Kubernetes-auth role (in `baoConfigureSteps`, alongside `eso`/`eso-pusher`)
binding the rotator SA to that policy — no new policy needed:

```go
{desc: "write kubernetes auth role linode-pat-rotator", fatal: true,
    args: []string{"write", "auth/kubernetes/role/linode-pat-rotator",
        "bound_service_account_names=linode-pat-rotator",
        "bound_service_account_namespaces=llz-linode-pat-rotator",
        "policies=secret-propagator", "ttl=15m"}},
```

### Command outline — `llz ci rotate-linode-pat`

Thin wrapper over the existing primitives; no new rotation logic.

```
runCIRotateLinodePAT:
  cur   := os.Getenv("LINODE_TOKEN")          // from the ESO-synced Secret
  cli   := linode.NewClient(cur)
  toks  := cli.ListProfileTokens(ctx)         // existing
  if newestByLabel(toks, label).ageDays < rotateAfterDays { log "not due"; return }
  new   := cli.CreateProfileToken(ctx, label, scopes, FmtLinodeTS(now+validity))  // existing
  if !probeOK(new.token) { return err }       // verify-before-revoke
  baoKVPut("secret/linode/api-token", {"token": new.token})   // k8s-auth write
  drainOld(cli, toks, label, keepN, grace)    // DeleteProfileToken, existing
```

(`credentials_pat.go`'s `create`/`revoke-old` already implement steps 2 and 5; the
new command reuses that logic minus the GitHub-secret write, plus the verify probe
and the k8s-auth OpenBao write.)

## Bootstrap / cold-start

The **first** in-cluster token is still seeded once at bootstrap (as today — the
labeler can't run without a token, and the rotator needs a token to mint the
next). The rotator takes over rotation on its first due run. Standalone/HA: the
rotator runs per-cluster and owns that cluster's `secret/linode/api-token`.

## Failure modes

| Failure | Behavior |
|---|---|
| Mint fails (API error/quota) | old token untouched; CronJob fails; next run retries; rotation-age SLA alerts if overdue |
| New token doesn't authenticate | verify probe fails before any write/revoke → old token stays live |
| OpenBao write fails after mint | new token exists but unused → drained by keep-newest-N next run; old stays in OpenBao |
| Partial drain | keep-newest-N is idempotent; re-runs converge |
| Self-lockout | impossible by construction — old token is only revoked after the new one is verified AND written |

## Observability

A rotation-age SLA check (reuse `health.ClassifyRotationAge`, like
`health-loki-objkey-rotation`) on the newest `llz-incluster-<env>` token's age,
surfaced by `llz-scheduled-checks.yml`. This replaces the in-cluster token's share
of the retired CI rotation-health.

## What this retires (once enabled + e2e-validated)

- The in-cluster token's slice of `secret-rotation.yml` (`credentials pat` +
  `propagate-pat`) — the provisioning PAT's CI rotation stays.
- `llz ci propagate-pat` is no longer needed **for the in-cluster token** (it was
  the OIDC bridge CI used to write OpenBao; the CronJob writes directly).

## Rollout

1. Land this design + the skeleton component (disabled; not in any env).
2. Implement `llz ci rotate-linode-pat` (+ unit tests over the pure
   due/keep-newest-N/verify logic) and the `linode-pat-rotator` k8s-auth role.
3. Enable `spec.components.linodePatRotator` in the lab/e2e env; confirm a real
   rotation cycle (mint → verify → OpenBao → ESO → labeler still works → drain).
4. Split the provisioning vs in-cluster PATs (narrow the in-cluster scopes).
5. Retire the in-cluster token's CI rotation steps.

## Open questions

- **Scope split vs single token + GitHub App (#3).** This design assumes the
  split. If a single token is required, #4 depends on #3 for the GitHub-secret
  write. Decision needed before implementation.
- **CronJob vs operator.** CronJob chosen for simplicity; revisit only if multiple
  Linode credentials need coordinated rotation.
- **Grace window / keep-N** values — align with the existing `credentials pat`
  defaults (90-day validity, keep newest, drain past grace).
