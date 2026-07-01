# apl-values — shared apl-core base + thin per-env overlays

The manifests and apl-core values your clusters run are **shared, DRY, and
single-sourced** here. An environment is **not** a clone of a 30-file reference
overlay — it is a thin kustomization that references the shared tree and toggles
components on/off. Consumed by Terraform's `cluster-bootstrap` root
(`../terraform-iac-bootstrap/cluster-bootstrap`), which renders the per-env
`values.yaml` and installs apl-core, and by Argo CD, which syncs the manifest tree.

```
apl-values/
  _shared/                    # the single source of truth (template-managed)
    manifest/                 # the always-on base: platform-support AppProject,
      kustomization.yaml      #   cluster-foundation apps, plus the dns/ tree
      platform-support-project.yaml   #   applied out-of-band by `llz bootstrap dns`
      applications/
      dns/                    # cert-manager DNS-01 issuer (instance-wide; the
                              #   ACME email is rendered once into letsencrypt-*)
    values.yaml               # apl-core values base (identity/secrets tokenized)
  components/                 # one kustomize Component per toggleable component
    externalSecrets/  certManager/  openbao/  harbor/  observability/
    argoWorkflows/  argoEvents/  volumeLabeler/
  <env>/                      # GENERATED per env by `llz render` — a THIN overlay
    manifest/
      kustomization.yaml      #   resources: ../../_shared/manifest
                              #   components: ../../components/<enabled>...
      env-revision-configmap.yaml          # per-env git revision marker
      linode-volume-labeler-region-patch.yaml  # the ONE genuine per-env delta
    values.yaml               #   _shared/values.yaml + apps.<key>.enabled toggles
```

## An environment is a thin overlay, generated — never hand-cloned

The template ships `_shared/` + `components/` **once**. You do not copy a
reference overlay or maintain a fixed `lab/staging/primary/secondary` list. You
declare each environment in the [LandingZone spec](../../docs/landing-zone-spec.md)
and let `llz` generate its overlay:

```bash
llz env add <env>            # scaffolds environments/<env>.yaml, then renders
```

`llz render` writes only the **per-env delta** into `apl-values/<env>/`:

1. `manifest/kustomization.yaml` — a thin overlay: `resources: ../../_shared/manifest`
   plus a `components:` entry for each component enabled in `spec.components`. The
   resources themselves live ONCE under `_shared/` + `components/`, never copied.
2. `manifest/env-revision-configmap.yaml` — the git revision this env's in-repo
   Argo CD content tracks (read by `cluster-bootstrap` as a plan-time precondition).
3. `manifest/linode-volume-labeler-region-patch.yaml` — the volume-labeler
   `REGION_SHORT`, the one genuinely per-env manifest value (only when enabled).
4. `values.yaml` — the `_shared/values.yaml` base with `apps.<key>.enabled` set
   from the component toggles and the spec-owned identity/platform keys patched in.

Because the base is shared, an upstream fix to a manifest or to the apl-core values
lands **once** in `_shared/` or `components/<name>/` and every environment inherits
it on the next `llz render` — no per-env reconciliation, no drift between clones.

Identity values written as `${cluster_name}` / `${cluster_domain}`, and the other
`${...}` placeholders (secrets + infra outputs: repo creds, dns token, loki/harbor
object-store, coredns IP), are rendered by Terraform `templatefile()` at
cluster-bootstrap. `spec.dns.acmeEmail`, being instance-wide, is rendered ONCE by
`llz render` into `_shared/manifest/dns/letsencrypt-clusterissuer.yaml`; the
remaining `REPLACE_PER_ENV` / `REPLACE_ME` placeholders (e.g. the cert-manager
webhook chart repoURL) are filled in by you — `llz doctor --env <env>` flags any
that survive.
