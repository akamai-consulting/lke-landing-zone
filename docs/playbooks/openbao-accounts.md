# OpenBao Accounts — Playbook

**Applies to:** OpenBao on every regional cluster (`<release>-openbao-0/1/2` in the `llz-openbao` namespace). No external Ingress — all access is via `kubectl exec` into the pod.

**Related:** [`docs/secrets.md`](../secrets.md) (architecture + dual-write), [`docs/runbooks/bootstrap-openbao.md`](../runbooks/bootstrap-openbao.md) (initial bootstrap), [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go) (auth methods + policies definition).

---

## Auth model

OpenBao in this deployment has three auth methods enabled (`llz ci bao-configure`):

| Method | Used by |
|---|---|
| **`token`** (root) | Operators for break-glass admin. Root token is **deleted** after bootstrap and re-issued via `bao operator generate-root` (requires 3 of 5 unseal-key holders) — see [`docs/secrets.md`](../secrets.md). |
| **`kubernetes`** | ESO's `ClusterSecretStore openbao` authenticates by its in-cluster ServiceAccount token via the `eso` Kubernetes-auth role, bound to the read-only `platform-ci` policy. No long-lived credential is stored. |
| **`jwt`** (GitHub-OIDC) | The `secret-propagator` role, used by `llz ci rotate-incluster-pat` to write `secret/linode/api-token`. CI authenticates with the workflow's GitHub OIDC token — no static credential. |

There is no LDAP, userpass, or AppRole. Humans use root tokens (emergency-only) or the operator-side dual-write scripts (`llz openbao set`, `llz openbao get`) which require a region-scoped operator token from `bao operator generate-root`.

---

## Human account — operator (break-glass)

You cannot create per-person OpenBao logins in this deployment. Operators authenticate by reconstituting a root token through the recovery-key quorum (Shamir-split recovery shares from `bao operator init`).

### When you need root access

Run `bao operator generate-root` on the cluster you need to touch (each region has its own keyspace):

```bash
# 1. Open a shell into the OpenBao leader
kubectl -n llz-openbao exec -it <release>-openbao-0 -- sh

# 2. Inside the pod:
export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_SKIP_VERIFY=true
bao operator generate-root -init
# → prints an OTP and a nonce — write them down
```

Then collect three recovery-key holders (under static-seal auto-unseal,
`generate-root` is authorized by the recovery keys from `bao operator init`). Each runs:

```bash
bao operator generate-root -nonce=<NONCE>
# → enters their recovery key share when prompted
```

After the third share, the command prints an **encoded root token**. Decode it locally:

```bash
bao operator generate-root -decode=<ENCODED> -otp=<OTP>
# → prints the live root token
```

The token is valid until the next bootstrap rotates the master key. Treat it as a single-use credential — `bao token revoke -self` when you're done.

### What an operator with root can do

Anything. Use root sparingly:

- `bao policy list`, `bao policy read <name>`, `bao policy write <name> -` — inspect/edit policies
- `bao kv put|get|delete secret/...` — read/write KV directly (prefer `llz openbao set` for dual-region writes — it enforces atomicity)
- `bao auth enable <method>` — only ever needed during cluster bring-up; if you're enabling new auth methods on a live cluster, update [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go) too so the next bootstrap reproduces the state.

---

## Machine account — Kubernetes auth (recommended pattern)

Use Kubernetes auth for any in-cluster workload that needs read access to OpenBao. The pod authenticates by its projected ServiceAccount token — there is no secret_id lifecycle to manage. ESO's `eso` role (bound to the read-only `platform-ci` policy) is the existing template.

### Adding a new Kubernetes-auth role

1. **Write a policy** — enumerate the exact KV paths the new principal reads (no wildcards):

    ```bash
    llz openbao exec -- policy write <policy-name> - <<'POLICY'
    path "secret/data/<your-path>"     { capabilities = ["read"] }
    path "secret/metadata/<your-path>" { capabilities = ["read", "list"] }
    POLICY
    ```

    Also add the policy + paths to [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go) (so the next bootstrap reproduces it) and to the ExternalSecret-path validation used by the lint job (so it covers any new ExternalSecret refs).

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

After any deletion, remove the corresponding policy + binding from [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go) so a future bootstrap doesn't re-create the principal.
