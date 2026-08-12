# Getting kubectl at a red e2e lane

When an `llz ci assert-*` lane fails in release-e2e and its log does not tell you
enough, you need to run commands against the cluster it failed on. This is how,
and the awkward parts are the reason this file exists.

## The two things that do not work

**Your own Linode token is the wrong account.** The e2e clusters are created with
`LINODE_API_TOKEN` from the instance repo's `infra-e2e` GitHub Environment. A
personal token — the one `linode-cli` is configured with, or a stale `LINODE_TOKEN`
in your shell — authenticates fine and then 404s on the cluster:

```console
$ linode-cli lke cluster-view 638084
Request failed: 404 … Not found
```

That 404 is an authorization answer wearing a not-found costume. Do not read it as
"the cluster is gone".

**The cluster is destroyed by the time you look.** The lane tears down on every
path, including failure. Dispatch with `keep_cluster: true` to keep it — the
teardown step then shows `skipped` while the job still reports success, so read
the step, not the job.

```console
gh workflow run release-e2e.yml --ref <branch> \
  -f dry_run=false -f keep_cluster=true
```

**It bills until you remove it**, and manual teardown needs the confirm token and
only targets the cluster module — databases and object storage persist between
runs by design:

```console
gh workflow run terraform.yml -R akamai-consulting/lke-landing-zone-example \
  -f action=destroy -f module=cluster -f region=e2e \
  -f confirm_destroy=destroy:e2e:cluster
```

Without `confirm_destroy` every destroy job fails its guard. That is safe —
nothing is partially torn down — but it looks like a broken teardown.

## What does work: borrow the instance repo's cluster access

`cluster-health.yml` in the instance repo already does the hard part — it reads the
kubeconfig from Terraform state and opens the runner's IP in the LKE-E
control-plane ACL. Put a read-only step in front of it on a branch, and dispatch
that branch.

```console
git clone --depth 1 git@github.com:akamai-consulting/lke-landing-zone-example.git
cd lke-landing-zone-example
git checkout -b debug/<what-you-are-chasing>
```

Add a step to `.github/workflows/llz-cluster-health.yml`, immediately **after**
`Cluster access (kubeconfig + runner ACL + llz)` and before `Cluster health`:

```yaml
      - name: DIAGNOSTICS (read-only)
        continue-on-error: true
        run: |
          set +e
          export KUBECONFIG="$HOME/.kube/config"
          echo "::group::whatever you need"
          kubectl get secret -A --no-headers | grep -i obj
          echo "::endgroup::"
          exit 0
```

Then:

```console
git push origin debug/<what-you-are-chasing>
gh workflow run cluster-health.yml -R akamai-consulting/lke-landing-zone-example \
  --ref debug/<what-you-are-chasing> \
  -f region=e2e -f fail-on-unhealthy=false
```

`--ref` runs that branch's version of the workflow, and the nested `./` reusable
resolves against the same ref, so your edit takes effect. **Delete the branch when
you are done** — the instance repo's default branch is overwritten by every e2e
instantiate, but stray branches are not.

### Two things that will waste a round

- **Port-forwards race.** `kubectl port-forward … & sleep 6` is not enough on a
  cold ACL. Poll until the target answers, and if you add a readiness probe make
  sure the endpoint you poll actually exists — a `/ready` that 404s makes a retry
  loop that never succeeds and then kills the forward it was waiting on.
- **The step logs are not readable until the job finishes.** `gh run view --job
  <id> --log` returns almost nothing mid-run even for steps that have completed.
  Wait for the job.

## Read the producer's config, not just the consumer's error

The failures that cost the most rounds were all the same shape: the gate named
what it could not find, and the answer was what the *other* side was actually
doing.

- Loki's tenant came from apl-core's collector config in namespace `otel`
  (`kubectl -n otel get cm -o yaml`), not from anything Loki said.
- The object-storage Secret names came from `kubectl get secret -A`, not from the
  ref that was missing.
- Harbor's mTLS mode came from `kubectl -n harbor get peerauthentication -o yaml`
  — it ships PERMISSIVE on purpose (ADR 0010 step 3), so a plaintext dial
  succeeding is correct behaviour and not a finding.

If a lane says "X is absent", the next command is almost always "what is present".
Several gates now print that themselves; where one does not, that is a gap worth
closing rather than a cluster worth standing up.
