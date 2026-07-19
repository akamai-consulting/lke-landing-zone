# apl-values — apl-core values base + thin per-env overlays

The apl-core values your clusters run are **shared, DRY, and single-sourced**
here. An environment is **not** a clone of a reference overlay — it is a thin
kustomization that references the shared platform tree and toggles components
on/off.

Consumed by `llz ci bootstrap-cluster`, which reads the per-env
`<env>/values.yaml` (written by `llz render`) and installs apl-core, and by
Argo CD, which syncs the manifest tree. There is no longer a `cluster-bootstrap`
Terraform root — Terraform owns day-0 infrastructure only (ADR 0002).

## What lives here

```
apl-values/
  values.yaml               # the apl-core values BASE (identity/secrets tokenized)
  <env>/                    # GENERATED per env by `llz render` — a THIN overlay
    manifest/
      kustomization.yaml    #   remote-refs the shared base + enabled components
      instance-custom.yaml  #   the escape hatch's ApplicationSet (carries this repo)
      env-revision-configmap.yaml   # per-env git revision marker
      <carved-app>.yaml             # one health-inert Application CR per carved component
    apps/<carved-app>/              # that component's self-contained per-env source root
      kustomization.yaml            #   remote-refs the shared Component
      <name>-env-patch.yaml         #   its per-env patch (e.g. llz-reconciler-env-patch.yaml)
    values.yaml             #   the base + apps.<key>.enabled toggles
```

Your own Kubernetes manifests do **not** live here — they live at
[`kubernetes-custom/`](../kubernetes-custom/) in the repo root. This directory is
apl-core's inputs only.

**The heavy platform manifests are NOT here.** The always-on base and the
per-component kustomize Components live at [`platform-apl/`](../../platform-apl/)
in the **template repo root**, outside the instance scaffold. An instance vendors
none of it: each env's `manifest/kustomization.yaml` references them as pinned
kustomize **remote refs** at the template ref the instance tracks, e.g.

```
resources:
  - github.com/akamai-consulting/lke-landing-zone//platform-apl/manifest?ref=v1.2.3&timeout=80
components:
  - github.com/akamai-consulting/lke-landing-zone//platform-apl/components/openbao?ref=v1.2.3&timeout=80
```

Argo CD's repo-server fetches them transitively when it builds this repo's App.
See `tools/internal/clusterspec/kustomize.go` (`RemoteBase`, `sharedManifestRef`).

## An environment is a thin overlay, generated — never hand-cloned

You do not copy a reference overlay or maintain a fixed `lab/staging/primary`
list. You declare each environment in the
LandingZone spec (`docs/landing-zone-spec.md` in the template repo — see
`docs/README.md` for the version-pinned link) and let `llz` generate it:

```bash
llz env add <env>            # scaffolds environments/<env>.yaml, then renders
```

`llz render` writes only the **per-env delta** into `apl-values/<env>/`:

1. `manifest/kustomization.yaml` — remote-refs to the shared base plus a
   `components:` entry per component enabled in `spec.components`.
2. `manifest/instance-custom.yaml` — the escape hatch's ApplicationSet. It
   carries this instance's repo URL + pinned revision, so it is emitted locally
   rather than fetched from the (instance-agnostic) shared base.
3. `manifest/env-revision-configmap.yaml` — the git revision this env's in-repo
   Argo CD content tracks (checked by `llz ci bootstrap-cluster` before install).
4. `manifest/<carved-app>.yaml` plus `apps/<carved-app>/…-env-patch.yaml` — each
   carved component's health-inert Application CR and its per-env patch, e.g.
   `apps/llz-reconciler/llz-reconciler-env-patch.yaml` (`REGION_SHORT` for volume
   labels plus `REGION`/`OBJ_CLUSTER` for linode-creds), emitted only when that
   component is enabled.
5. `values.yaml` — the `apl-values/values.yaml` base with `apps.<key>.enabled` set
   from the component toggles and the spec-owned identity/platform keys patched in.

An upstream fix lands **once** in `platform-apl/` and every environment inherits
it on the next `llz upgrade` (which re-pins the ref) — no per-env reconciliation,
no drift between clones.

## Placeholders

Identity values (`${cluster_name}`, `${cluster_domain}`) and the other `${...}`
tokens (secrets + infra outputs: repo creds, dns token, loki/harbor object-store,
coredns IP) are substituted by `llz ci bootstrap-cluster` at install time.
`spec.dns.acmeEmail`, being instance-wide, is applied by a JSON6902 patch in the
per-env overlay onto the shared `llz-letsencrypt-*` ClusterIssuers. Any remaining
`REPLACE_PER_ENV` / `REPLACE_ME` placeholder is yours to fill — `llz doctor --env
<env>` flags the survivors.

## Your own resources — `kubernetes-custom/`

`kubernetes-custom/` is the operator escape hatch: drop your Kubernetes manifests
there and Argo CD applies them. It is `owned` (see `.template-manifest`) — the
template ships it once and never touches it again.

Its layout mirrors the App Platform GitOps convention
(https://techdocs.akamai.com/app-platform/docs/gitops): `namespaces/<ns>/` for
namespaced resources (one Argo CD Application per directory, namespace
auto-created) and `global/` for cluster-scoped ones. See
`docs/extending-llz.md` → "Your own Kubernetes resources" in the template repo
for the full contract.

## The values repo has a second branch — don't confuse the two

apl-core runs in BYO-git mode against **this same repo**, but on a separate,
machine-owned branch (`apl-<env>`), where apl-operator pushes its own rendered
`env/` tree and platform SealedSecrets. That tree is apl-core's, not yours:
`main` holds the human-authored IaC + `apl-values/` source you are reading now.
Never hand-edit `apl-<env>`. See
`docs/designs/apl-core-values-branch-isolation.md` in the template repo.
