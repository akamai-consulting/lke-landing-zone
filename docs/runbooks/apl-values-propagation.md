# apl-values propagation (external HTTPS git)

The in-cluster Gitea is obsoleted. apl-core's `otomi.git` points at the
**instance repo over HTTPS+PAT** (`apl_values_repo_token`, Contents: write), and
apl-operator reads AND writes its rendered values tree there directly. There is
no longer a Terraform `push-apl-values` step, no `enable_apl_values_auto_push`
gate, and no `otomi/values` Gitea repo to seed.

## How a values change reaches the cluster

`apl-values/<env>/values.yaml` feeds two consumers:

1. **`llz ci bootstrap-cluster`** — the file is substituted (identity
   placeholders, tokens) and passed as the apl-core chart values on every
   bootstrap. This sets `otomi.git.*` (the external repo + PAT), `apps.*`,
   `cluster.*`, and `dns.*`. It does NOT set `apps.loki.adminPassword`: apl-core
   declares that as an x-secret with a generator, and from 6.2.0 it is no longer
   in `required`, so apl-core generates and self-wires it. (Up to 6.1.0 the two
   flags contradicted each other — the field was required but only fillable
   inside otomi's own bootstrap — which is why older instances rendered a value
   here.)

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

## One-time repair: apl-core apps the overlay used to overwrite

`llz` renders a per-env `apl-overlay/apps.yaml` fragment and the in-cluster
apl-overlay reconciler merges it onto apl-core's own `env/apps/<name>.yaml` on
the `apl-<env>` branch. Until the fix described below, that fragment named apps
the landing zone does not own on managed clusters:

| App | What the overlay wrote | Why it is wrong |
| --- | --- | --- |
| `gitea` | `enabled: false` | on managed, apl-core's in-cluster gitea is the values-repo backend the overlay is delivered through |
| `kyverno`, `policy-reporter` | `enabled: true` | apl-core's, via the `policyEngine` component, which is `ManagedSkip` |
| `trivy` | `enabled: true` | apl-core's, via `imageScanning`, likewise `ManagedSkip` |

The renderer now omits every app whose component does not emit on managed, so
nothing rewrites these again — **but omitting a key does not undo one already
committed.** An instance that ran an affected release still carries the values on
its `apl-<env>` branch, and the reconciler will never touch those files again.

Check whether yours does, per deployment:

```bash
# Requires a checkout of the instance repo with the apl-<env> branch fetched.
git fetch origin 'refs/heads/apl-*:refs/remotes/origin/apl-*'
for app in gitea kyverno policy-reporter trivy; do
  echo "== $app"
  git show "origin/apl-<env>:env/apps/$app.yaml" 2>/dev/null || echo "  (absent — nothing to repair)"
done
```

Repair is a hand edit on that branch, because it is apl-core's file and the
landing zone has just declared it has no opinion about it — having `llz` write
the correction would re-assert the ownership the fix removed. For each file that
exists and that LLZ set:

- `gitea` — restore `spec.enabled: true` if the cluster uses apl-core's
  in-cluster gitea as its values-repo backend. If Argo CD is already syncing from
  the external HTTPS repo (the normal LLZ posture, verified above), gitea being
  off is not currently breaking anything and can be left alone.
- `kyverno` / `policy-reporter` / `trivy` — set these to whatever the App
  Platform Console says the operator actually wants. The overlay forced them on;
  `enabled: true` may well be correct, in which case there is nothing to do.

Nothing here is urgent unless `gitea` is the live backend. The reason to check is
that the values are now unmanaged in both directions: no longer written by LLZ,
and not restored by apl-operator either.
