# `llz-openbao`

Opinionated OpenBao/Vault bootstrap: a KV v2 secret engine, AppRole +
Kubernetes auth methods, a read policy for CI/ESO, and the AppRole-rotation
wiring (a Kubernetes-auth role that mints/destroys secret_ids for the CI role).

Extracted from the `openbao-config/` root config. The
org-specific surface — role names, the pinned `role_id`, and the secret tree the
CI policy grants — is **variabilized**, so a sibling system team supplies its own
without editing HCL. The CI read policy is **generated** from `ci_read_paths`
(not a hard-coded heredoc), which is what makes it reusable.

> **API compatibility:** uses the `hashicorp/vault` provider, which is
> API-compatible with OpenBao at the OSS feature level.

## What it deploys

- `vault_mount.kv` — KV v2 engine at `var.kv_path` (default `secret`)
- `vault_auth_backend` — `approle` + `kubernetes`
- `vault_policy.ci` — read grants generated from `ci_read_paths`
- `vault_policy.rotator` — secret-id management for the CI role
- `vault_approle_auth_backend_role.ci` — the CI AppRole (pinned `role_id`)
- `vault_kubernetes_auth_backend_role.rotator` — rotation role bound to a SA

## Inputs (all defaulted to the in-repo deployment's values)

| Variable | Default | Description |
|---|---|---|
| `kubernetes_host` | `https://kubernetes.default.svc:443` | API server for the K8s auth method |
| `kv_path` | `secret` | KV v2 mount path |
| `ci_role_name` | `platform-ci` | CI AppRole + read-policy name (org identity) |
| `ci_role_id` | `""` → `ci_role_name` | Pinned role_id; must match the ESO ClusterSecretStore |
| `ci_read_paths` | _15-path list_ | KV sub-paths the CI role may read (the org secret tree) |
| `ci_token_ttl` / `ci_token_max_ttl` | `15m` / `30m` | CI token TTLs |
| `ci_secret_id_ttl` | `2208h` | 92 days — outlasts the quarterly rotation interval |
| `rotator_role_name` | `approle-rotator` | rotation role + policy name |
| `rotator_service_account_names` | `["approle-rotator"]` | bound SAs |
| `rotator_service_account_namespaces` | `["openbao"]` | bound SA namespaces |
| `rotator_token_ttl` | `15m` | rotation token TTL |

## Outputs

`approle_role_id`, `ci_role_name`, `next_steps` (post-apply secret-id seeding +
root-token revocation checklist).

## Provider

Inherits the `vault` provider from the calling root (configure
`provider "vault" { address = …; token = … }` there).

## Usage

```hcl
provider "vault" {
  address = "https://localhost:8200"
  token   = var.openbao_token
}

module "openbao_config" {
  source = "git::ssh://git@github.com/akamai-consulting/lke-landing-zone.git//terraform-modules/llz-openbao?ref=v0.1.0"

  ci_role_name  = "acme-ci"
  ci_read_paths = ["myapp/config", "registry/creds"]
}
```

See [`modules/RELEASING.md`](../RELEASING.md) for the tag/version contract.
