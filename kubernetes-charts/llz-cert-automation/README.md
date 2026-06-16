# llz-cert-automation

Event-driven TLS-certificate renewal for the edge HAProxy, built on **Argo
Events** + **Argo Workflows**.

## Why this chart exists

The edge HAProxy container has **no SIGHUP TLS-reload** (verified in practice).
cert-manager rotates the `haproxy-tls` Secret every ~80 days, but a running
HAProxy keeps serving the **old** certificate until its image is rebuilt and
redeployed. This chart automates that rebuild the moment the cert rotates.

## How it works

```
cert-manager rotates haproxy-tls Secret
  → EventSource (apiserver resource watch)  publishes UPDATE to the EventBus
    → EventBus "default" (JetStream, 3 replicas, at-least-once delivery)
      → Sensor  submits the haproxy-rebuild Workflow (image-tag = Secret resourceVersion)
        → Workflow:
            extract-cert  (kubectl: pull tls.crt/tls.key from cert-manager)
            build-image   (buildah: FROM haproxy:2.9-alpine, COPY cert, push to Harbor)
            deploy-haproxy    (gh CLI: dispatch the GitHub Actions deploy workflow)
```

The EventSource filters to cert-manager-managed Secrets
(`controller.cert-manager.io/fao=true`) and `afterStart: true` so a controller
restart can't trigger a spurious rebuild.

## Apply-ordering (sync-wave) contract

These waves are operational scars baked in as defaults — change with care:

| Resource | Wave | Why |
|---|---|---|
| Namespace | `-20` | Must exist before anything targets it. |
| NetworkPolicies | `-10` | Applied before any eventbus/eventsource/sensor/workflow pod starts. |
| EventBus / EventSource / Sensor | `-14` | AFTER argo-events installs its CRDs (its App is at `-15`) and BEFORE wave-0 workloads that need the EventBus. **EventBus must apply before the EventSource/Sensor that reference it via `eventBusName: default`** — the source kustomization ordering enforces this too. |

PSA on the namespace is `baseline` (not `restricted`) because the buildah
build-image step runs as root to construct overlay layers; audit/warn stay
`restricted` to track the gap. NetworkPolicy apiserver egress lists both `443`
and `6443` per the LKE-E post-DNAT Cilium quirk.

## Install

```sh
helm install llz-cert-automation oci://ghcr.io/akamai-consulting/charts/llz-cert-automation \
  --version 0.1.0
```

In this repo it is consumed by an Argo CD Application referencing the published
OCI chart (replacing the raw manifests under
`apl-values/_shared/manifest/cert-automation/`).

## Values

| Key | Default | Description |
|---|---|---|
| `nameOverride` / `fullnameOverride` | `""` | Standard Helm name overrides. |
| `namespace` | `llz-cert-automation` | Namespace all resources run in (created by the chart). |
| `certManagerNamespace` | `cert-manager` | Namespace of the watched TLS Secret. |
| `eventBus.name` | `default` | EventBus name — must match every `eventBusName` reference. |
| `eventBus.jetstream.version` | `latest` | JetStream version. |
| `eventBus.jetstream.replicas` | `3` | JetStream replicas (at-least-once delivery). |
| `watchedSecret.name` | `haproxy-tls` | cert-manager Secret whose rotation drives a rebuild. |
| `harborUrl` | `https://harbor.<cluster-domain>:5000` | Harbor push destination (set to the cluster's Harbor host). |
| `haproxyImage.baseImage` | `haproxy:2.9-alpine` | Base image the rebuild builds FROM. |
| `haproxyImage.repoPath` | `platform/haproxy` | Repo path under `harborUrl` for the rebuilt image. |
| `githubDeploy.repo` | `your-org/your-instance-repo` | Repo whose Actions deploy workflow is dispatched. |
| `githubDeploy.ref` | `main` | Git ref for the dispatch. |
| `githubDeploy.workflow` | `deploy-haproxy.yml` | Workflow file dispatched. |
| `images.kubectl` | `registry.k8s.io/kubectl:v1.32.4@sha256:…` | extract-cert step image (digest-pinned). |
| `images.buildah` | `quay.io/buildah/stable:v1.41` | build-image step image. |
| `images.ghCli` | `ghcr.io/cli/cli:v2.65.0` | deploy-haproxy step image. |
| `externalSecrets.refreshInterval` | `1h` | ESO refresh interval. |
| `externalSecrets.secretStore.kind` | `ClusterSecretStore` | Secret backend kind. |
| `externalSecrets.secretStore.name` | `openbao` | Secret backend name. |
| `externalSecrets.githubToken.secretName` | `cert-automation-github-token` | K8s Secret for the GH dispatch token. |
| `externalSecrets.githubToken.remoteKey` | `cert-automation/github-token` | Backend key (no mount prefix). |
| `externalSecrets.githubToken.remoteProperty` | `token` | Backend property + secret key. |
| `externalSecrets.harborDockerConfig.secretName` | `harbor-docker-config` | K8s Secret for the Harbor docker config. |
| `externalSecrets.harborDockerConfig.remoteKey` | `harbor/docker-config` | Backend key (no mount prefix). |
| `externalSecrets.harborDockerConfig.remoteProperty` | `config_json` | Backend property. |
| `networkPolicy.certManagerNamespace` | `cert-manager` | Namespace-label egress target for the workflow. |
| `networkPolicy.harborNamespace` | `harbor` | Namespace-label egress target for the workflow. |
| `networkPolicy.apiserverPorts` | `[443, 6443]` | apiserver egress ports (both required on LKE-E). |
| `syncWaves.namespace` | `"-20"` | Namespace sync-wave. |
| `syncWaves.networkPolicy` | `"-10"` | NetworkPolicy sync-wave. |
| `syncWaves.argoEvents` | `"-14"` | EventBus/EventSource/Sensor sync-wave. |
| `podSecurity.*` | enforce `baseline`, audit/warn `restricted` (v1.31) | Namespace PSA labels. |
