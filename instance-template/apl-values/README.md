# apl-values — per-environment apl-core overlays

Each environment in your instance is one overlay directory here, consumed by
Terraform's `cluster-bootstrap` root (`../terraform-iac-bootstrap/cluster-bootstrap`) which
renders the per-env `values.yaml` and installs apl-core.

```
apl-values/
  example/            # the reference overlay shipped by the template
    values.yaml       # apl-core values (cluster identity tokenized)
    manifest/         # this env's Argo Apps / raw manifests (instance-owned)
  <env>/              # created per environment from example/ (not hardcoded)
```

## Environments are created dynamically — not hardcoded

The template ships **only** `example/`. You do not edit a fixed list of
`lab/staging/primary/secondary` directories; you generate each environment from
the example with the scaffold:

```bash
llz env add <env> --template-env example \
  --region <linode-region> --cluster-domain <env>.example.internal
```

That command:

1. Copies `apl-values/example/` → `apl-values/<env>/`, substituting the env name
   and identity tokens.
2. Generates the matching Terraform tfvars under `../terraform/<root>/<env>.tfvars`
   from the example tfvars.

Identity values written as `${cluster_name}` / `${cluster_domain}`, and the other
`${...}` placeholders (secrets + infra outputs: repo creds, dns token, loki/harbor
object-store, coredns IP), are rendered by Terraform `templatefile()` at
cluster-bootstrap; values written `REPLACE_PER_ENV` are filled in by you (or the
scaffold flags) per environment. For a **spec-driven** instance (`landingzone.yaml`
present), `llz render` additionally writes the identity + platform keys
(`cluster.name`/`domainSuffix`, `dns.domainFilters`, `otomi.has*`) straight from
the spec, so those are already resolved before Terraform runs.
