# Adding the internal-CIDR firewall (Akamai employees)

The Linode internal-CIDR Cloud Firewall controller is an **Akamai-internal** feature.
It needs the Linode `firewall-templates` v4beta API scope (internal accounts only) and
its rule data is internal infrastructure detail, so it lives in the private repo
[`akamai-consulting/lke-landing-zone-internal`](https://github.com/akamai-consulting/lke-landing-zone-internal)
— **not** in this public template.

This public repo keeps the pieces that are safe to ship and that drive the feature:

- `terraform-modules/llz-node-firewall/` — *creates* the Linode Cloud Firewall and attaches it to the node pool.
- `llz ci bootstrap-cloud-firewall` — seeds `LINODE_FIREWALL_ID` / `LKE_CLUSTER_ID` into the controller's ConfigMap.
- `llz ci health` (`checkFirewallBootstrap`) — health-checks that ConfigMap.
- `llz runner-acl` — punches the CI runner egress IP into the control-plane ACL.

You add the **controller + chart** back from the private repo. Three steps.

## Prerequisites

- Read access to `akamai-consulting/lke-landing-zone-internal` and pull access to its
  published artifacts in `ghcr.io/akamai-consulting`:
  - chart `oci://ghcr.io/akamai-consulting/charts/llz-linode-cidr-firewall`
  - image `ghcr.io/akamai-consulting/firewall-controller-internal` (the `-internal`
    suffix avoids colliding with the public org's existing `firewall-controller` package)
- A Linode API token **with the `firewall-templates` scope** (internal accounts). Without
  it the controller falls back to its committed offline ruleset.
- A released `vX.Y.Z` from the private repo (cut the release there **first** — see its
  `RELEASES.md` — then pin the version below). The initial release is **`v0.0.1`**.

## 1. Deploy the chart (Argo Application)

Add an Application to the shared `apl-values/_shared/manifest/applications/` (instance-
wide — it lands in every env) and list it in `apl-values/_shared/manifest/kustomization.yaml`
alongside `applications/cluster-foundation.yaml`. (The shared tree is template-managed,
so re-assert the `resources:` entry after a `copier update` — or, better, contribute the
app upstream as its own kustomize Component under `apl-values/components/`.)

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: llz-linode-cidr-firewall
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "0"
  finalizers:
    - resources-finalizer.argocd.argoproj.io
spec:
  project: platform-support
  source:
    repoURL: ghcr.io/akamai-consulting/charts
    chart: llz-linode-cidr-firewall
    targetRevision: v0.0.1            # the released chart version (initial release)
    helm:
      releaseName: llz-linode-cidr-firewall
  destination:
    server: https://kubernetes.default.svc
    namespace: kube-system
  syncPolicy:
    automated: { prune: false, selfHeal: true }
    syncOptions: [ServerSideApply=true]
```

Or, if you drive apps through the `llz-argo-bootstrap-apps` chart, add a wave entry in
its values (mirrors the other OCI component entries):

```yaml
  - name: llz-linode-cidr-firewall
    enabled: true
    syncWave: "0"
    destinationNamespace: kube-system
    source:
      type: oci
      repoURL: ghcr.io/akamai-consulting/charts
      chart: llz-linode-cidr-firewall
      targetRevision: v0.0.1
```

## 2. Seed the controller config

The chart renders its ConfigMap (`llz-linode-cidr-firewall-config`) with empty
`LINODE_FIREWALL_ID` / `LKE_CLUSTER_ID` placeholders. Seed them with the public CLI
(already in this repo) after the cluster is up:

```bash
llz ci bootstrap-cloud-firewall      # patches the ConfigMap; survives Argo selfHeal (SSA)
```

Set `FIREWALL_TEMPLATE_ID` in the chart values to your Linode firewall-template ID
(default is the `example-non-prod` placeholder).

## 3. Verify

```bash
llz ci health        # checkFirewallBootstrap confirms the ConfigMap + keys are present
```

## Notes

- **Image pull:** mark/keep the `ghcr.io/akamai-consulting/firewall-controller-internal` package
  accessible to your cluster's pull secret, or mirror it to Harbor.
- **No internal scope?** The controller still reconciles using its committed offline
  fallback ruleset (example data in the public scaffold; seed the real map privately —
  see the private repo's README → *Seeding real data*).
- **Version lockstep:** the private repo tags chart + image together; pin both to the
  same `vX.Y.Z`.
