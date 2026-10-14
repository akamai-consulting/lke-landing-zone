# Runbook — the first build failed

> **Who this is for:** you ran `llz up <env> --yes` (or `llz build <env> --yes`),
> the GitHub Actions run went red, and you need to know whether your instance is
> broken, whether you can just run it again, and what got left behind.
>
> **Short answer:** re-dispatching is almost always the right move. Terraform
> state is authoritative and every stage of the build is idempotent, so a re-run
> continues from where it stopped rather than starting over.

<!-- toc -->
## Contents

- [1. Find the failure](#1-find-the-failure)
- [2. Which stage failed, and what exists now](#2-which-stage-failed-and-what-exists-now)
- [3. Fix, publish, re-dispatch](#3-fix-publish-re-dispatch)
- [4. Common first-build failures](#4-common-first-build-failures)
- [5. Sweep what a failed cycle left behind](#5-sweep-what-a-failed-cycle-left-behind)
- [6. Starting clean](#6-starting-clean)

<!-- /toc -->

## 1. Find the failure

`llz build` prints the run URL and a `gh run watch` line when it dispatches. If
you have lost it:

```bash
gh run list --workflow terraform.yml --limit 5     # from the instance repo
gh run view <run-id> --log-failed                  # just the failing steps
```

The job also writes a **recovery summary to the run's Summary tab** naming the
stage, what exists at that point, and the commands below. Read that first — it is
written by the failing job itself and knows which stage died.

## 2. Which stage failed, and what exists now

| Stage | What exists when it fails | Re-runnable? |
|---|---|---|
| **Apply Shared VPC** | Nothing cluster-shaped. Every cheap preflight lives in this job and runs *before* any cloud mutation, so a failure here is almost always configuration. | Yes — fix the config first |
| **Apply Cluster** | Possibly a partial LKE-E cluster, VPC, firewall, node pool. Terraform state records whatever landed. | Yes — the apply continues |
| **Apply Object Storage** | The cluster exists; buckets may be partial. | Yes |
| **Bootstrap OpenBao** | Cluster + buckets exist. apl-core install, the convergence gate, or the OpenBao seed failed. | Yes — seeded paths are skipped, `helm upgrade --install` is idempotent |

**Nothing here requires a teardown.** A destroy costs you another full build; see
[§6](#6-starting-clean) for when it is genuinely warranted.

## 3. Fix, publish, re-dispatch

The order matters, and step 2 is the one people skip:

```bash
# 1. fix whatever the failing step named, in your instance repo
# 2. PUBLISH it — the build renders from the pushed default branch, not your
#    working tree. An uncommitted fix is not in the next run.
git add -A && git commit -m "fix: <what>" && git push
# 3. re-dispatch
llz up <env> --yes          # re-runs tokens → doctor → build
# ...or skip straight to the apply if the gates were already green:
llz build <env> --yes
```

`llz up` is the safer choice after a failure: it re-checks the readiness gates,
and `llz tokens` skips everything already provisioned.

## 4. Common first-build failures

| Symptom | Cause | Fix |
|---|---|---|
| `llz ci assert-image-fresh` fails with **"no usable version stamp"** | The `llz` in the image `TF_IMAGE` names reports no build stamp (or one that names no commit), so the skew guard has nothing to compare. **Not** an old image — an image old enough to predate the stamping fix runs an older `llz` that cannot print this at all | The image build is broken upstream: report it, quoting the stamp in the message (whitespace included — it distinguishes a bad `-X` path from a bad build-arg). If `TF_IMAGE`/`KUBE_IMAGE` are simply behind your pin, `llz tokens --env <env> --yes` re-pins both |
| `llz ci assert-image-fresh` fails | `TF_IMAGE`/`KUBE_IMAGE` name a different commit than your template pin — normal right after `llz upgrade` | `llz tokens --env <env> --yes` re-pins and pushes both |
| `llz render --check` fails | The committed `apl-values/` no longer match the spec + pin | `llz render <env>` locally, then commit + push |
| `require-secret` / `validate-tokens` fails | A missing, expired, or under-scoped credential | `llz doctor --env <env>` reports the same set locally |
| Environment-scoped `gh secret set` 401s | `OPENBAO_SECRETS_WRITE_TOKEN` lacks **Environments: write** (the fine-grained "Secrets" permission is *not* enough for these), or you are not Environment admin on `infra-<env>` | Re-mint the PAT with Actions + Environments + Secrets write |
| `harbor-robot-provisioner` Jobs fail every 5 min with `check repo secret HARBOR_ROBOT_NAME: HTTP 403`, and converge hard-fails on them | The same `OPENBAO_SECRETS_WRITE_TOKEN` lacks **Secrets: write** — the *repo-level* permission, which "Environments" does not grant. The PAT is live and passes every other check | Re-mint the PAT with Actions + Environments + Secrets write, re-run bootstrap so `llz ci bao-seed-all` reseeds `secret/infra/github-dispatch-token`. `llz doctor --env <env>` reports this in the PERMS column |
| `assert-apl-version` fails | The spec pins an apl-core chart below the 6.x floor | **Remove** `cluster.bootstrap.aplChartVersion` from `environments/<env>.yaml` so it tracks the baseline this llz release targets. Raising it to a specific version also works, but `llz upgrade` will drop that pin again if it names a version llz itself once set |
| `assert-k8s-version` fails | The spec pins an LKE-Enterprise version **this Linode account** cannot build. `llz env add` seeds the pin from that account's own catalog and re-checks it whenever another deployment is added, so this means the catalog has rotated since **this** deployment was scaffolded (or you pinned `--k8s-version` yourself, or scaffolded with no `LINODE_TOKEN` and got the compiled fallback). A deployment that already runs the pin is exempt, so what usually reaches here is a re-create — and `llz env add` now asks the same question at scaffold time, pinning what an existing cluster runs rather than re-deriving, so a re-scaffold no longer arrives here with a version its own cluster is not on. Availability is per-account and moves: the same pin was accepted by one account and rejected by another in the same hour, so a release note cannot answer it. A pin whose *spelling* is off fails here too — the catalog names full build ids and terraform sends the pin verbatim, so `v1.34.6` (no `+lke`) and `1.34.6+lke2` (no `v`) are both rejected. For the leading-`v` case the failure names the exact entry you meant | Set `cluster.k8sVersion` to one of the versions the failure lists — **in whichever file holds it**: `landingzone.yaml` under `spec.defaults` (where every deployment inherits it, and where the scaffold seeds it) or `environments/<env>.yaml` to override one deployment. The failure names which; fixing the per-deployment file when the value is shared unblocks one deployment and leaves the rest failing one dispatch at a time. Unfixed, the cluster apply dies ~15 minutes in with `[400] [k8s_version] k8s_version is not valid`. A cluster that **already runs** the pin is exempt — it plans no diff — so this only fires when the version is about to be sent to the API |
| `preflight failed: N orphaned resource(s) over threshold` | Real orphans, from earlier failed cycles. **New:** the Volume census used to be structurally inert (the preflight was passed a deployment name where it wanted a Linode region, so no Volume ever matched and the count always read 0). It now counts, so an account with a pre-existing backlog fails here on its first apply after upgrading | [§5](#5-sweep-what-a-failed-cycle-left-behind). To unblock without sweeping, set the repo variable `PREFLIGHT_FAIL_ON_ORPHANS=false` — but the backlog is what makes cluster-create hang, so sweep it |
| Cluster create **hangs**, then times out | Account quota — VPCs or vCPUs — or a backlog of orphans from an earlier failed cycle | [§5](#5-sweep-what-a-failed-cycle-left-behind), then re-dispatch. Set `PREFLIGHT_VPC_LIMIT` / `PREFLIGHT_VCPU_LIMIT` to your account's limits so the next one fails fast instead of hanging |
| `[400] … already exists` on a bucket | Linode bucket labels share one namespace per region **across accounts** | Change `spec.instance.objLabelPrefix` in `landingzone.yaml` — not the deployment name |
| `assert-upgrade-plan` fails the object-storage apply with bucket **renames** | The reverse of the row above, and it only reaches instances that already exist. `spec.instance.objLabelPrefix` moved the bucket prefix off the module's old hardcoded `platform` onto a per-instance value, and a bucket label is create-time only — so for an instance provisioned under `platform-*` the plan is `2 to add, 0 to change, 2 to destroy` against buckets holding your Loki chunks and Harbor images. The apply is refused before it reaches the API. Without the check, Linode's own `[400] … is not empty` was the only thing stopping it, so a deployment whose buckets happened to be **empty** lost them silently | Keep the buckets you have: pin `spec.instance.objLabelPrefix` in `landingzone.yaml` to the prefix the failure names (`platform` for an instance scaffolded before v0.0.44), then re-dispatch — the plan goes empty. The prefix also names this instance's Object Storage **key** labels, which `llz reap` and the rotation table match exactly, so check those first. Taking the new names instead means copying the objects across and repointing Loki and Harbor before anything is deleted |
| Converge never completes | Usually one Argo Application; the job dumps the app states and the cert-manager CA chain on failure | Read the `diagnose-argocd` output in the failed job |

## 5. Sweep what a failed cycle left behind

A cancelled or failed run can strand NodeBalancers, VPCs and Volumes whose
cluster is gone. They cost money, and a backlog of them makes the **next**
cluster-create hang on account quota — which looks like a totally different
failure.

```bash
export LINODE_TOKEN=…                        # or LINODE_API_TOKEN
llz reap --region <linode-region>            # DRY RUN — lists what it would delete
llz reap --region <linode-region> --yes      # actually delete
```

Add `--cluster-label <label>` to also reap an orphaned cluster plus its node
firewall and VPC, and `--env <env>` to reap that deployment's minted Linode
credentials. `llz reap --help` documents the full scope.

> **The `region` here is the Linode region (`us-sea`), not the deployment name** —
> `llz reap` is the exception to the rest of the CLI, where `--region` means the
> deployment. Passing a deployment name used to match nothing and report
> `deleted=0`, which reads as a clean account; `llz reap` now refuses it and tells
> you that deployment's Linode region.

## 6. Starting clean

Only when the instance is genuinely unrecoverable — a wrong region, a wrong
object-storage cluster, a deployment name you want to abandon. It costs a full
rebuild.

```bash
gh workflow run terraform.yml \
  -f action=destroy -f module=all -f region=<env> \
  -f confirm_destroy=destroy:<env>:cluster
```

`confirm_destroy` is **mandatory** — without it every destroy job fails its
guard, by design. The token is `destroy:<region>:<module>`.

Then sweep any leftovers ([§5](#5-sweep-what-a-failed-cycle-left-behind)) and
re-dispatch the build.

> **Do not delete the Terraform state bucket** to "start clean". The state is
> encrypted with `TF_STATE_ENCRYPTION_PASSPHRASE` and is the only record of what
> exists; destroying it strands every resource it tracks.

## See also

- [Quick start](../quickstart.md) — the flow this runbook interrupts
- [OpenBao bootstrap runbook](bootstrap-openbao.md) — the bootstrap stage in detail
- [apl-values propagation](apl-values-propagation.md) — when Argo is not syncing what you pushed
