# OpenBao Accounts — Playbook

**Applies to:** OpenBao on every regional cluster (`<release>-openbao-0/1/2` in the `llz-openbao` namespace). No external Ingress — access is via `llz openbao …` (which port-forwards for you) or `kubectl exec` into the pod.

**Related:** [`docs/runbooks/openbao-team-login.md`](../runbooks/openbao-team-login.md) (the per-person login — start there), [`docs/secrets.md`](../secrets.md) (architecture + dual-write), [`docs/runbooks/bootstrap-openbao.md`](../runbooks/bootstrap-openbao.md) (initial bootstrap + break-glass), [`llz ci bao-configure`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/tools/internal/extensions/lifecycle/identityconfig/openbao_configure.go) (auth methods + policies definition).

---

## Auth model

OpenBao in this deployment has four auth methods enabled (`llz ci bao-configure`):

| Method | Used by |
|---|---|
| **`keycloak`** (JWT/OIDC) | **The everyday human path.** A `<name>-writer` role per declared `spec.teams` entry, bound on the APL Keycloak `groups` claim. Operators get a short-lived, attributed, team-scoped token via `llz openbao login --team <name>` — no root, no keys. See [`docs/runbooks/openbao-team-login.md`](../runbooks/openbao-team-login.md). |
| **`token`** (root) | Break-glass only. Root is **revoked** at the end of every bootstrap and re-issued from the recovery quorum — see below. |
| **`kubernetes`** | ESO's `ClusterSecretStore openbao` authenticates by its in-cluster ServiceAccount token via the `eso` Kubernetes-auth role, bound to the read-only `platform-ci` policy **plus a `<name>-reader` policy for each declared `spec.teams` entry** (read on that team's `openbaoSubtree`), so ESO can sync team-written app secrets. No long-lived credential is stored. |
| **`jwt`** (GitHub-OIDC) | The `secret-propagator` role, used by `llz ci rotate-incluster-pat` to write `secret/linode/api-token`. CI authenticates with the workflow's GitHub OIDC token — no static credential. |

There is no LDAP, userpass, or AppRole.

---

## Human account — team login (the normal path)

**Per-person OpenBao access exists.** An operator who belongs to an APL team logs
in with their Keycloak identity and gets a token carrying only that team's
`<name>-writer` policy:

```bash
eval "$(llz openbao login --team <name>)"     # browser device flow → OPENBAO_TOKEN
llz openbao set secret/<name>/<path> key=value --yes
```

The write is **attributed** (the Keycloak `sub`) and **least-privilege** (that
team's subtree only). A write outside the subtree gets a 403. `login` needs
kubectl reach to the target cluster — it port-forwards OpenBao for the token
exchange. Tokens live 1h (max 8h); re-run `login` when one expires.

Onboarding a person to a team, declaring a new team, and the whole troubleshooting
matrix live in [`docs/runbooks/openbao-team-login.md`](../runbooks/openbao-team-login.md).
`llz openbao get/set/exec` print a warning whenever they fall back to
`OPENBAO_ROOT_TOKEN`, pointing back here.

> **A cluster bootstrapped before the `keycloak` mount existed does not have it.**
> `bao-configure` is root-gated and runs only on a bootstrap or a deliberate
> re-configure, so on an older cluster the team path is unavailable until one runs
> — see the retrofit section of the team-login runbook. That is the case where the
> break-glass path below is the only option.

---

## Human account — break-glass root

Root access is for auth/policy admin and incidents, not for writing secrets. Two
ways to get it, in order of preference:

### 1. The break-glass workflow (you almost certainly want this)

It reconstitutes root from the recovery quorum **stored in the `infra-<env>` GitHub
environment**, so it needs no operator-held keys, and returns the token encrypted
to your key rather than printing it into a run log:

```bash
gh workflow run breakglass-openbao.yml --field region=<env> --field action=generate \
  --field recipient_pubkey_b64="$(base64 < bg-pub.pem | tr -d '\n')"
```

Full lifecycle (keypair creation, decrypt, `revoke` when you are done):
[`docs/runbooks/bootstrap-openbao.md`](../runbooks/bootstrap-openbao.md) →
"Break-glass root token".

### 2. `generate-root` by hand — only if you personally hold 3 of the 5 recovery keys

The recovery keys are printed **once** to the first bootstrap's job summary and are
not distributed to operators. If you do not have three of them in hand, use the
workflow above instead — this path cannot be completed.

`llz openbao regen-root <env>` drives the whole quorum flow for you (shares are read
in terminal raw mode, never echoed or written to disk) and verifies the resulting
token resolves to a root policy:

```bash
# kubectl must point at the target cluster; each region has its own keyspace.
llz openbao regen-root <env>
llz openbao regen-root <env> --update-gha-secret [--repo owner/repo]   # also seed infra-<env>
```

<details>
<summary>Driving <code>bao operator generate-root</code> in the pod by hand</summary>

Only if `llz openbao regen-root` itself is unavailable. **The environment below is
load-bearing** — OpenBao serves two listeners and the in-pod one is not the obvious
port:

```text
[::]:8200        pod network — mTLS, a CLIENT CERTIFICATE IS REQUIRED
127.0.0.1:8210   loopback    — TLS, no client certificate
```

An in-pod `bao` invocation holds no client identity, so it must target **`:8210`**.
And it must set the **`BAO_*`** names: OpenBao prefers a present `BAO_*` variable
over its `VAULT_*` alias unconditionally, and the chart bakes `BAO_ADDR` into the
container — so exporting `VAULT_ADDR` alone is silently ignored and the command
reaches the mTLS listener without a certificate, failing with
`http2: client conn could not be established`.

```bash
# 1. Open a shell into the OpenBao leader
kubectl -n llz-openbao exec -it <release>-openbao-0 -- sh

# 2. Inside the pod — the loopback listener, verifying the server (no SKIP_VERIFY):
export BAO_ADDR=https://127.0.0.1:8210   BAO_CACERT=/openbao/tls/ca.crt
export VAULT_ADDR="$BAO_ADDR"            VAULT_CACERT="$BAO_CACERT"   # older `bao`/`vault`
bao operator generate-root -init
# → prints an OTP and a nonce — write them down
```

Then collect three recovery-key holders (under static-seal auto-unseal,
`generate-root` is authorized by the recovery keys from `bao operator init`). Each
runs, with the same environment:

```bash
bao operator generate-root -nonce=<NONCE>
# → enters their recovery key share when prompted
```

After the third share, the command prints an **encoded root token**. Decode it:

```bash
bao operator generate-root -decode=<ENCODED> -otp=<OTP>
# → prints the live root token
```

</details>

**OpenBao root tokens have no TTL** — they do not self-expire, so lifecycle is
manual. Treat one as single-use: `bao token revoke -self` when you are done, or
`gh workflow run breakglass-openbao.yml --field region=<env> --field action=revoke`,
which also deletes the `infra-<env>::OPENBAO_ROOT_TOKEN` secret.

### What an operator with root can do

Anything. Use root sparingly:

- `bao policy list`, `bao policy read <name>`, `bao policy write <name> -` — inspect/edit policies
- `bao kv put|get|delete secret/...` — read/write KV directly (prefer `llz openbao set` for dual-region writes — it enforces atomicity)
- `bao auth enable <method>` — only ever needed during cluster bring-up; if you're enabling new auth methods on a live cluster, update [`llz ci bao-configure`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/tools/internal/extensions/lifecycle/identityconfig/openbao_configure.go) too so the next bootstrap reproduces the state.

---

## Machine account — Kubernetes auth (recommended pattern)

Use Kubernetes auth for any in-cluster workload that needs read access to OpenBao. The pod authenticates by its projected ServiceAccount token — there is no secret_id lifecycle to manage. ESO's `eso` role (bound to the read-only `platform-ci` policy, plus a `<name>-reader` policy per declared team so it can read each team's subtree) is the existing template.

### Adding a new Kubernetes-auth role

1. **Write a policy** — enumerate the exact KV paths the new principal reads (no wildcards):

    ```bash
    llz openbao exec -- policy write <policy-name> - <<'POLICY'
    path "secret/data/<your-path>"     { capabilities = ["read"] }
    path "secret/metadata/<your-path>" { capabilities = ["read", "list"] }
    POLICY
    ```

    Also add the policy + paths to [`llz ci bao-configure`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/tools/internal/extensions/lifecycle/identityconfig/openbao_configure.go) (so the next bootstrap reproduces it) and to the ExternalSecret-path validation used by the lint job (so it covers any new ExternalSecret refs).

2. **Bind the role to a ServiceAccount**:

    ```bash
    llz openbao exec -- write auth/kubernetes/role/<role-name> \
      bound_service_account_names=<sa-name> \
      bound_service_account_namespaces=<ns> \
      policies=<policy-name> \
      ttl=15m
    ```

The pod authenticates with its projected SA token; OpenBao validates against the cluster's TokenReview API (configured by `llz ci bao-configure`). The `eso` role used by the ESO ClusterSecretStore is the canonical example.

---

## Rotation + removal

| Action | Command |
|---|---|
| Update a role's policy/SA binding | `llz openbao exec -- write auth/kubernetes/role/<role> ...` (re-write with the new fields) |
| Delete a policy | `llz openbao exec -- policy delete <policy-name>` (remove all bindings first) |
| Drop a Kubernetes-auth role | `llz openbao exec -- delete auth/kubernetes/role/<role>` |

After any deletion, remove the corresponding policy + binding from [`llz ci bao-configure`](https://github.com/akamai-consulting/lke-landing-zone/blob/main/tools/internal/extensions/lifecycle/identityconfig/openbao_configure.go) so a future bootstrap doesn't re-create the principal.
