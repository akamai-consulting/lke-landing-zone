# `kubernetes-custom/global/` — your cluster-scoped resources

Resources that belong to no namespace: CRDs, ClusterRoles, ClusterRoleBindings,
StorageClasses, ValidatingWebhookConfigurations, and the like. Synced by the
`instance-custom-global` Argo CD Application. This mirrors App Platform's GitOps
convention: https://techdocs.akamai.com/app-platform/docs/gitops

```
global/
  my-crd.yaml
  my-clusterrole.yaml
  my-storageclass.yaml
```

Subdirectories are organizational only — everything here is recursed and applied.

**Namespaced resources do not belong here.** Without a namespace of their own they
would land in `default`. Put them under `namespaces/<ns>/` instead.

This directory is empty until you add to it. This README is a placeholder so the
directory exists in git — you can delete it once you have real content.
