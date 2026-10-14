# apl-values — the apl-core inputs LLZ owns

apl-core's own `values.yaml` is **Linode's** on the managed App Platform. What
lives here is the much smaller set of inputs LLZ owns: the per-env Argo CD
`manifest/` tree, and the **apl-overlay** — object storage, app toggles, and the
per-app chart values LLZ asserts — which the in-cluster reconciler merges onto
apl-core's machine-owned `apl-<env>` branch.

An environment is **not** a clone of a reference overlay — it is a thin
kustomization that references the shared platform tree and toggles components
on/off.

Consumed by Argo CD, which syncs the `manifest/` tree, and by the in-cluster
apl-overlay reconciler. There is no longer a `cluster-bootstrap` Terraform root —
Terraform owns day-0 infrastructure only (ADR 0002).

## What lives here

> **There is no `values.yaml` here, and that is the point.** On the managed App
> Platform Linode owns apl-core's values; `llz render` emits none, and the scaffold
> check fails the build if one ever appears. The single channel LLZ has into an
> apl-core app's chart values is `_shared/apl-overlay/appvalues.yaml` — see below.

```
apl-values/
  _shared/apl-overlay/      # the obj + app-toggle + app-values overlay (managed)
  <env>/                    # GENERATED per env by `llz render` — a THIN overlay
    manifest/
      kustomization.yaml    #   remote-refs the shared base + enabled components
      instance-custom.yaml  #   the escape hatch's ApplicationSet (carries this repo)
      env-revision-configmap.yaml   # per-env git revision marker
      <carved-app>.yaml             # one health-inert Application CR per carved component
    apps/<carved-app>/              # that component's self-contained per-env source root
      kustomization.yaml            #   remote-refs the shared Component
      <name>-env-patch.yaml         #   its per-env patch (e.g. llz-reconciler-env-patch.yaml)
```

Your own Kubernetes manifests do **not** live here — they live at
[`kubernetes-custom/`](../kubernetes-custom/) in the repo root. This directory is
apl-core's inputs only.

**The heavy platform manifests are NOT here.** The always-on base and the
per-component kustomize Components live at
[`platform-apl/`](https://github.com/akamai-consulting/lke-landing-zone/tree/main/platform-apl)
in the **template repo**, outside the instance scaffold — so there is no local path
to follow, which is why that link leaves this repo. An instance vendors
none of it: each env's `manifest/kustomization.yaml` references them as pinned
kustomize **remote refs** at the template ref the instance tracks, e.g.

```
resources:
  - github.com/akamai-consulting/lke-landing-zone//platform-apl/manifest?ref=v1.2.3&timeout=80
components:
  - github.com/akamai-consulting/lke-landing-zone//platform-apl/components/openbao?ref=v1.2.3&timeout=80
```

Argo CD's repo-server fetches them transitively when it builds this repo's App.
See `tools/internal/shared/clusterspec/kustomize.go` (`RemoteBase`, `sharedManifestRef`).

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
5. `apl-overlay/` — the per-env obj + app-toggle layers that merge onto the
   `_shared` base (`llz render` writes both; the reconciler merges them onto
   apl-core's machine branch).

An upstream fix lands **once** in `platform-apl/` and every environment inherits
it on the next `llz upgrade` (which re-pins the ref) — no per-env reconciliation,
no drift between clones.

## Placeholders

The overlay carries exactly one placeholder — `${obj_access_key_id}` in
`_shared/apl-overlay/obj.yaml` — which the in-cluster apl-overlay reconciler fills
from OpenBao (`secret/obj/platform`) before it writes to the machine branch. It
never resolves on `main`, so nothing but a placeholder is committed here. The
paired secret never transits git at all: ESO writes it straight into the
`obj-secrets` Secret.

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
