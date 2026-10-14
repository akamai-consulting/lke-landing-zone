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
| `kyverno`, `policy-reporter` | `enabled: true` | apl-core's, via the `policyEngine` component, which is `ManagedSkip` |
| `trivy` | `enabled: true` | apl-core's, via `imageScanning`, likewise `ManagedSkip` |

The renderer now omits every app whose COMPONENT does not emit on managed, so
nothing rewrites the three above again.

It is not every app the landing zone writes about, and it should not be. The
instance-wide `_shared` layer still sets `enabled: false` for seven apl-core apps
— `gitea`, `knative`, `kserve`, `kubeflow-pipelines`, `linode-cfw`, `rabbitmq`,
`tekton` — and that layer is a standing platform decision rather than a component
toggle. Enabling one of those in the App Platform Console **is** reverted on the
next reconcile pass, deliberately.

`gitea` is on that list for a specific reason: apl-core 6.x gates
`apl-gitea-operator` on `gitea.enabled`, and with BYO-Git that operator's clone
path points at a repo the platform does not use. It also carries an unencrypted
`gitea-valkey` PVC. LLZ disables it on every managed cluster, and
the retired apl-core values base — which was **not** rendered on managed, and has
since been deleted for that reason — documented that intent all along. The
`_shared` apps overlay is where it lives now.

**But omitting a key does not undo one already committed.** An instance that ran
an affected release still carries `kyverno`, `policy-reporter` and `trivy` forced
on, on its `apl-<env>` branch, and the reconciler will never touch those files
again.

Check whether yours does, per deployment:

```bash
# Run from a checkout of the instance repo. The branch is resolved from the
# deployment name rather than pasted: `apl-<env>` as a literal made every
# `git show` fail identically to an absent file, so the check reported all four
# apps clean on exactly the cluster it exists to find. git's own fetch error is
# shown rather than replaced with a guess at which failure it was.
# A FUNCTION, not a bare script: `exit 1` in a snippet you paste into your own
# shell closes the terminal. `return` leaves you where you were.
check_apl_apps() {
  local env="$1" branch="origin/apl-$1"

  if ! git fetch origin "refs/heads/apl-$env:refs/remotes/$branch" 2>&1; then
    echo "fetch of apl-$env failed (see git's message above) — concluding nothing" >&2
    return 1
  fi
  if ! git rev-parse --verify --quiet "$branch" >/dev/null; then
    echo "$branch does not exist — apl-operator has not bootstrapped this env" >&2
    return 1
  fi

  for app in gitea kyverno policy-reporter trivy; do
    printf '== %s\n' "$app"
    if git cat-file -e "$branch:env/apps/$app.yaml" 2>/dev/null; then
      git show "$branch:env/apps/$app.yaml" | sed 's/^/  /'
    else
      echo "  (absent — nothing to repair)"
    fi
  done
}

check_apl_apps primary          # ← once per deployment
```

`git cat-file -e` is what separates the two answers: it asks whether the blob
exists and says so in its exit status, where a bare `git show … || echo absent`
reports "absent" just as readily for a branch you never fetched.

Repair is a hand edit on that branch, because it is apl-core's file and the
landing zone has just declared it has no opinion about it — having `llz` write
the correction would re-assert the ownership the fix removed. For each file that
exists and that LLZ set:

- `gitea` — **leave it disabled.** It is on the `_shared` list and LLZ still
  writes it off on every pass; that is the intended state on managed, for the
  reasons above. Included in the check only so you can confirm it is still off.
- `kyverno` / `policy-reporter` / `trivy` — set these to whatever the App
  Platform Console says the operator actually wants. The overlay forced them on;
  `enabled: true` may well be correct, in which case there is nothing to do.

Nothing here is urgent. The reason to check is that `kyverno`, `policy-reporter`
and `trivy` are now unmanaged in both directions — no longer written by LLZ, and
not restored by apl-operator either — so whatever value the overlay last forced
is the value they keep until someone sets it in the Console.
