# OpenBao Accounts — Playbook

**Applies to:** OpenBao on every regional cluster (`<release>-openbao-0/1/2` in the `openbao` namespace). No external Ingress — all access is via `kubectl exec` into the pod.

**Related:** [`docs/secrets.md`](../secrets.md) (architecture + dual-write), [`docs/runbooks/bootstrap-openbao.md`](../runbooks/bootstrap-openbao.md) (initial bootstrap), [`docs/runbooks/approle-rotation.md`](../runbooks/approle-rotation.md) (CI AppRole rotation), [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go) (auth methods + policies definition).

---

## Auth model

OpenBao in this deployment has three auth methods enabled (`llz ci bao-configure`):

| Method | Used by |
|---|---|
| **`token`** (root) | Operators for break-glass admin. Root token is **deleted** after bootstrap and re-issued via `bao operator generate-root` (requires 3 of 5 unseal-key holders) — see [`docs/secrets.md`](../secrets.md). |
| **`approle`** | The CI AppRole (`<instance>-ci`) used by ESO's `ClusterSecretStore openbao` (read-only KV). Rotation is quarterly via the Argo `approle-rotation` CronWorkflow. |
| **`kubernetes`** | The `approle-rotator` ServiceAccount in the `openbao` namespace — used only by the rotation CronWorkflow to mint new CI AppRole secret IDs. |

There is no LDAP, OIDC, or userpass. Humans use root tokens (emergency-only) or the operator-side dual-write scripts (`llz openbao set`, `llz openbao get`) which require a region-scoped operator token from `bao operator generate-root`.

---

## Human account — operator (break-glass)

You cannot create per-person OpenBao logins in this deployment. Operators authenticate by reconstituting a root token through Shamir's secret sharing.

### When you need root access

Run `bao operator generate-root` on the cluster you need to touch (each region has its own keyspace):

```bash
# 1. Open a shell into the OpenBao leader
kubectl -n openbao exec -it <release>-openbao-0 -- sh

# 2. Inside the pod:
export VAULT_ADDR=https://127.0.0.1:8200
export VAULT_SKIP_VERIFY=true
bao operator generate-root -init
# → prints an OTP and a nonce — write them down
```

Then collect three unseal-key holders. Each runs:

```bash
bao operator generate-root -nonce=<NONCE>
# → enters their unseal key share when prompted
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

## Machine account — AppRole (recommended pattern)

Use AppRole for any in-cluster workload that needs read access to OpenBao. The existing CI AppRole (`<instance>-ci`) is the template.

### Adding a new AppRole

1. **Write a policy** — enumerate the exact KV paths the new principal reads (no wildcards):

    ```bash
    llz openbao exec -- policy write <policy-name> - <<'POLICY'
    path "secret/data/<your-path>"     { capabilities = ["read"] }
    path "secret/metadata/<your-path>" { capabilities = ["read", "list"] }
    POLICY
    ```

    Also add the policy + paths to [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go) (so the next bootstrap reproduces it) and to the ExternalSecret-path validation used by the lint job (so it covers any new ExternalSecret refs).

2. **Create the AppRole**:

    ```bash
    llz openbao exec -- write auth/approle/role/<role-name> \
      token_policies=<policy-name> \
      token_ttl=15m \
      token_max_ttl=30m \
      secret_id_ttl=2208h     # 92 days — matches existing rotation cadence
    ```

3. **Pin the role_id** to a stable string (so consumers don't have to re-read it on each rotation):

    ```bash
    llz openbao exec -- write auth/approle/role/<role-name>/role-id \
      role_id=<role-name>
    ```

4. **Mint the first secret_id** and hand it to the consumer:

    ```bash
    llz openbao exec -- write -f auth/approle/role/<role-name>/secret-id
    # → returns secret_id + secret_id_accessor
    ```

    For ESO: write the secret_id into the appropriate region's GitHub environment secret (e.g. `OPENBAO_APPROLE_SECRET_ID_<MYAPP>`) and reference it from your `ClusterSecretStore`. Mirror the pattern in the OpenBao Argo application values under `instance-template/apl-values/example/manifest/openbao/` and the existing CI AppRole rotation workflow.

5. **Schedule rotation** — extend the `approle-rotation` CronWorkflow (under the OpenBao Argo application templates in `instance-template/apl-values/example/manifest/openbao/`) to mint a new secret_id every 60-90 days and push it to the GitHub environment secret. The existing CI AppRole rotation step is a copy-pasteable template.

### Adding a Kubernetes-auth role (alternative)

For workloads that authenticate by their pod ServiceAccount (skips the secret_id lifecycle entirely — recommended for net-new workloads):

```bash
llz openbao exec -- write auth/kubernetes/role/<role-name> \
  bound_service_account_names=<sa-name> \
  bound_service_account_namespaces=<ns> \
  policies=<policy-name> \
  ttl=15m
```

The pod authenticates with its projected SA token; OpenBao validates against the cluster's TokenReview API (configured by `llz ci bao-configure`). The `approle-rotator` is the only example today.

---

## Rotation + removal

| Action | Command |
|---|---|
| Rotate a secret_id (manual) | `llz openbao exec -- write -f auth/approle/role/<role>/secret-id` — then push the new value to the consumer |
| Rotate a secret_id (scheduled) | The Argo `approle-rotation` CronWorkflow handles the CI AppRole; clone the workflow step for new roles |
| Revoke a specific secret_id | `llz openbao exec -- write auth/approle/role/<role>/secret-id-accessor/destroy secret_id_accessor=<accessor>` |
| Delete an entire AppRole | `llz openbao exec -- delete auth/approle/role/<role>` |
| Delete a policy | `llz openbao exec -- policy delete <policy-name>` (remove all bindings first) |
| Drop a Kubernetes-auth role | `llz openbao exec -- delete auth/kubernetes/role/<role>` |

After any deletion, remove the corresponding policy + binding from [`llz ci bao-configure`](../../tools/cmd/llz/ci_openbao_configure.go) so a future bootstrap doesn't re-create the principal.
