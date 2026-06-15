# llz-eso-cert-watcher

Singleton watcher that rolling-restarts the External Secrets Operator (ESO) on
two signals that both leave ESO's cached Vault/OpenBao client stale until it is
restarted.

## Why this chart exists

**Signal 1 — CA rotation.** ESO's Vault provider (used for OpenBao via
`provider: vault`) reads the `caProvider`-referenced Secret **once** at client
creation and caches the CA for the pod's lifetime. When cert-manager rotates the
backing TLS Secret on its `renewBefore` schedule, ESO holds the **old** CA while
OpenBao serves a leaf signed by the **new** CA. Every `ExternalSecret` reconcile
then fails with `x509: certificate signed by unknown authority` and the
`ClusterSecretStore` flips to `Ready=False (InvalidProviderConfig)`. Detected by
polling the cert-manager `Certificate`'s `.status.notBefore`.

**Signal 2 — store wedged at bootstrap.** ESO validates a store **once** when it
creates the client. On a fresh bootstrap ESO is installed early and routinely
starts before its inputs (the AppRole `secretId`, or `openbao-tls`) exist; that
first validation fails and ESO does **not** re-validate when the inputs later
appear, so the store sticks at `Ready=False` and every `ExternalSecret` fails
until someone manually restarts ESO. Detected by polling the
`ClusterSecretStore`'s `Ready` condition; the recovery restart is
cooldown-limited (`watch.store.cooldownSeconds`) so a genuinely-misconfigured
store can't thrash ESO. Set `watch.store.name: ""` to disable this signal (the
store-read `ClusterRole` and the signal-2 loop are then omitted).

This chart deploys a tiny `Deployment` that polls both signals every
`pollIntervalSeconds` and runs `kubectl rollout restart` on the ESO `Deployment`
when either fires. Effective lag: 0–`pollIntervalSeconds` for rotation, one
cooldown window for a wedged store.

Both signals read only **public status** (the cert-manager `Certificate` and the
`ClusterSecretStore`), never a Secret's data, so the watcher needs no access to
`tls.key`.

> Although named for ESO, the mechanism is generic: it restarts any Deployment
> when a watched cert-manager Certificate rotates. Useful to anyone running ESO +
> cert-manager (or any client that caches a CA at startup).

## Install

```sh
helm install llz-eso-cert-watcher oci://ghcr.io/akamai-consulting/charts/llz-eso-cert-watcher \
  --version 0.2.0 \
  -n external-secrets
```

In this repo it is consumed by an Argo CD Application
(`apl-values/_shared/manifest/external-secrets/argocd/applications/eso-cert-watcher.yaml`)
referencing the published OCI chart.

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` | `alpine/k8s` | Watcher image (needs `kubectl` + a shell). |
| `image.tag` | `1.32.4` | Image tag. |
| `image.pullPolicy` | `IfNotPresent` | |
| `namespace` | `external-secrets` | Namespace the watcher + restart RBAC run in (where ESO lives). |
| `target.deployment.name` | `external-secrets` | Deployment to rolling-restart on rotation. |
| `target.deployment.namespace` | `""` (→ `namespace`) | Namespace of the target Deployment. |
| `watch.certificate.name` | `openbao-tls` | cert-manager Certificate to watch (signal 1). |
| `watch.certificate.namespace` | `openbao` | Namespace of the watched Certificate. |
| `watch.store.name` | `openbao` | ClusterSecretStore to watch (signal 2). `""` disables signal 2 + its ClusterRole. |
| `watch.store.cooldownSeconds` | `120` | Min seconds between store-wedge recovery restarts. |
| `pollIntervalSeconds` | `60` | Poll interval; worst-case detection lag. |
| `rbacSyncWave` | `"-5"` | Argo CD sync-wave for SA/Role/RoleBinding (before the wave-0 Deployment). |
| `resources` | see values.yaml | Container resources. |
