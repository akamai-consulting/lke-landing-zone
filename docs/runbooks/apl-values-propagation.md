# apl-values propagation (external HTTPS git)

The in-cluster Gitea is obsoleted. apl-core's `otomi.git` points at the
**instance repo over HTTPS+PAT** (`apl_values_repo_token`, Contents: write), and
apl-operator reads AND writes its rendered values tree there directly. There is
no longer a Terraform `push-apl-values` step, no `enable_apl_values_auto_push`
gate, and no `otomi/values` Gitea repo to seed.

## How a values change reaches the cluster

`instance-template/apl-values/<env>/values.yaml` feeds two consumers:

1. **`llz ci bootstrap-cluster`** — the file is substituted (identity
   placeholders, tokens) and passed as the apl-core chart values on every
   bootstrap. This sets `otomi.git.*` (the external repo + PAT), `apps.*`,
   `cluster.*`, and `dns.*`. Note `apps.loki.adminPassword` IS still rendered
   here: apl-core 6.x declares it as an x-secret with a generator, but that
   generator only runs inside otomi's own bootstrap — a raw `helm install apl/apl`
   validates the schema first and fails "adminPassword is required" if it is
   omitted.

2. **apl-operator** — on bootstrap it materialises its values tree in the
   external repo (the `env/`, `apps/`, … layout apl-core owns) using the same
   PAT, then reconciles continuously. Argo CD syncs the platform from that repo
   over HTTPS — no SSH keys, no Gitea readiness race.

So edits to `values.yaml` land in the cluster by re-running
`llz ci bootstrap-cluster` (which re-renders the chart values); ongoing
reconciliation is Argo-native against the external repo.

## Verifying

```bash
# apl-operator's resolved values-repo URL — should be the github.com HTTPS repo:
kubectl -n apl-operator get cm apl-git-config -o jsonpath='{.data.repoUrl}'; echo

# Argo CD repository Secrets (both point at the instance repo over HTTPS):
kubectl -n argocd get secret -l argocd.argoproj.io/secret-type=repository

# Platform Applications reconciled from the external repo:
kubectl -n argocd get applications
```

`llz verify` (check 5) asserts the `apl-git-config` repoUrl resolves to the
external HTTPS host (not Gitea).

## Credential

`APL_VALUES_REPO_TOKEN` (fine-grained GitHub PAT, **Contents: write** on the
instance repo) is the single credential for both apl-core's `otomi.git.password`
and the Argo CD repository Secrets. Provisioned by `llz tokens` and rotated like
the other GitHub PATs (see
[linode-credential-rotation.md](linode-credential-rotation.md)).
