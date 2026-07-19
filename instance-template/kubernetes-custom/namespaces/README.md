# `kubernetes-custom/namespaces/` — your namespaced resources

One directory per Kubernetes namespace. Each becomes its **own Argo CD
Application** (`instance-custom-<namespace>`), synced into a namespace of that
name, created automatically if absent. This mirrors App Platform's GitOps
convention: https://techdocs.akamai.com/app-platform/docs/gitops

```
namespaces/
  my-app/                 # → Application instance-custom-my-app → namespace my-app
    deployment.yaml
    service.yaml
    networkpolicy.yaml
    externalsecret.yaml   # the openbao ClusterSecretStore is ready at wave 10
  argocd/                 # → your own Argo CD Application CRs live here
    my-helm-app.yaml
```

Subdirectories inside a namespace directory are organizational only — everything
is recursed and applied into that namespace.

## Reserved names

- **`apl-*`** — apl-core owns these namespaces and manages them with its own
  `gitops-ns-apl-*` Applications. A directory here would put two Argo CD
  Applications in contention over the same resources. Rejected by `llz render`
  and `llz doctor`.
- **`global`** — would collide with the Application generated from `kubernetes-custom/global/`.
  Put cluster-scoped resources in `kubernetes-custom/global/` instead.

This directory is empty until you add to it; the ApplicationSet simply generates
no Applications. This README is a placeholder so the directory exists in git —
you can delete it once you have real content.
