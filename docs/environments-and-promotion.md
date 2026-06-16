# Environments as a code-promotion pipeline

How to use llz **deployments** (the `<env>` you pass to `llz`) to model a
`dev → staging → prod` promotion pipeline: one instance repo, one set of
workflows, N ranked deployments, and a change that walks them in order on green.

> Prerequisite: you can stand up a single deployment end to end
> ([quickstart.md](quickstart.md)). This doc is the multi-deployment layer on
> top — read [§0 of the quickstart](quickstart.md#0-a-word-on-environments-first)
> for the three meanings of "environment" first; here, "environment" always means
> a **deployment**.

## 1. The model: a pipeline is N deployments in one repo

An llz instance repo describes any number of deployments. Each is one cluster's
identity — its own Terraform state key, `cluster/<env>.tfvars`, and
`apl-values/<env>/` overlay — discovered dynamically from the tfvars (there is no
hardcoded env list). A **code-promotion pipeline is just a set of those
deployments put in an order**: `dev`, then `staging`, then `prod`.

What "promotion" means here is deliberately narrow and GitOps-shaped:

- **The change lives in git.** A pull request edits the shared sources — a chart
  pin in `apl-values/*/values.yaml`, a module change under
  `terraform-iac-bootstrap/`, a workflow — and merges to `main`. `main` is the
  single source every deployment builds from.
- **Promotion is applying that already-merged change to the next deployment.**
  You build `dev` from `main`, verify it converges, then build `staging` from the
  *same* `main`, then `prod`. The artifact being promoted is the commit; the
  pipeline is the order you roll it out in.
- **Each deployment keeps its own knobs.** Region, node sizing, k8s version, HA
  role, domain, and chart-version pins are per-`<env>.tfvars`, so `prod` can lag
  `dev` on a version while still sharing the repo (see §5).

This gives you blast-radius control (a bad change is caught on `dev` before it
reaches `prod`) without a second repo or a separate config system: the same
`llz validate → tokens → build → status` flow you already run for one deployment,
run per stage in a fixed order.

```
        one instance repo, main branch
                     │
   PR merges a chart pin / module change to main
                     │
   ┌─────────────────┼──────────────────┐
   ▼                 ▼                  ▼
 dev (rank 1) ──▶ staging (rank 2) ──▶ prod (rank 3)
 build+verify     build+verify         build+verify
   on green         on green             on green
```

## 2. Declaring the order: `promotion_rank`

The pipeline order is declared the same way HA topology is — a field in the
cluster tfvars that Terraform carries and `llz` reads to drive CI (this is the
"tfvars is the single source of truth" contract):

```hcl
# terraform-iac-bootstrap/cluster/dev.tfvars
promotion_rank = 1
# staging.tfvars → 2,  prod.tfvars → 3
```

Rules:

- **Ascending = promotion order.** Lowest positive rank is the first stage,
  highest is the last.
- **`0` (the default) means "not in any pipeline."** Existing deployments,
  one-off `lab`/`scratch` clusters, and the `e2e` deployment stay out until you
  rank them — promotion is explicit opt-in, nothing changes for deployments you
  don't touch.
- **Ranks must be unique.** A pipeline is a line, not a tie, so "what's next" is
  unambiguous — `llz` errors loudly if two deployments share a rank.
- **Gaps are fine.** Use `10, 20, 30` if you want room to insert a stage later.

Set it at scaffold time:

```bash
llz env add dev     --region us-ord --promotion-rank 1   # ...plus the usual flags
llz env add staging --region us-ord --promotion-rank 2
llz env add prod    --region us-sea --promotion-rank 3
```

…or edit `promotion_rank` in an existing `cluster/<env>.tfvars` by hand. Either
way it is a reviewable line in a committed tfvars file.

## 3. Reading the order: `llz env list --ordered` and `llz env next`

Two read-only helpers turn the ranks into something CI can walk. Both are
layout-aware and read straight from the tfvars, so they never drift from what is
actually scaffolded.

```bash
# The pipeline, in promotion order (only ranked deployments appear):
$ llz env list --ordered
dev
staging
prod

# As a JSON array — drops straight into a workflow matrix via fromJSON(...):
$ llz env list --ordered --json
["dev","staging","prod"]

# The stage promoted into after a given one — what a promote-on-green job
# builds next once <env> is green:
$ llz env next dev
staging
$ llz env next staging
prod
$ llz env next prod
llz: deployment "prod" is the last stage — nothing to promote to   # non-zero exit
```

`llz env next <env>` errors (non-zero) on the last stage and on an unranked
deployment — both are "stop here" signals a CI step can branch on.

## 4. Wiring it into CI: a generated, GitHub-native pipeline

The pipeline runs as a **static `needs:`-chained workflow**
(`.github/workflows/promote.yml`) — *generated from the ranks*, not hand-written.
GitHub already provides the three pieces of a promotion pipeline, so the runtime
reinvents nothing:

| Promotion concern | Native mechanism |
|---|---|
| On-green gate between stages | `needs:` — a stage starts only once the prior stage's whole apply **and** the `converge` gate succeeded |
| Approval + soak time | `infra-<stage>` Environment protection rules (required reviewers + wait timer) |
| "Only `main` promotes" | Environment deployment-branch policy (set to `main`) |
| Resume from a failed stage | GitHub's built-in **"Re-run failed jobs"** |

`promotion_rank` stays the single source of truth; the workflow is rendered from
it. Each stage calls the same reusable `llz-terraform.yml` apply path the
single-deployment flow uses — promotion only adds *ordering* (`needs:`) and the
*green gate* between stages:

```yaml
# .github/workflows/promote.yml  (GENERATED — `llz env pipeline` renders it)
jobs:
  dev:                                                    # rank 1 — pipeline entry
    uses: <org>/lke-landing-zone/.github/workflows/llz-terraform.yml@vX.Y.Z
    with: { action: apply, module: all, region: dev }
    secrets: inherit
  staging:                                                # rank 2
    needs: dev                                            # green gate
    uses: <org>/lke-landing-zone/.github/workflows/llz-terraform.yml@vX.Y.Z
    with: { action: apply, module: all, region: staging }
    secrets: inherit
  prod:                                                   # rank 3
    needs: staging
    uses: <org>/lke-landing-zone/.github/workflows/llz-terraform.yml@vX.Y.Z
    with: { action: apply, module: all, region: prod }
    secrets: inherit
```

You never edit this file by hand. **`llz env add <env> --promotion-rank N`
regenerates it**, and for the hand-edit path (you changed a `promotion_rank` in a
tfvars directly) **`llz env pipeline`** re-renders it from the current ranks:

```bash
llz env pipeline           # regenerate promote.yml from the promotion_rank ordering
llz env pipeline --check    # CI gate: exit non-zero if promote.yml has drifted from the ranks
```

Wire `llz env pipeline --check` into the instance's CI as the "did you
regenerate?" guard so a tfvars rank edit can't silently diverge from the workflow.
The reusable-workflow pin (`uses:@<ref>` + `template-ref` + `forge_flavor`) is
**preserved** from the file already on disk (or lifted from the sibling
`terraform.yml`, finally the `forge_flavor` copier answer), so a Renovate version
bump is *not* treated as drift — only a rank change is. Carrying `forge_flavor`
keeps a GHE instance's promotion stages gating the same CI workarounds the
single-deployment `terraform.yml` does.

Start a rollout with **Run workflow** on the Promote action (or `gh workflow run
promote.yml`). It walks `dev → staging → prod`, pausing at each protected
environment for approval, and stopping at whichever stage fails its convergence
gate. Adding a stage is a one-line tfvars edit (`promotion_rank`) plus
`llz env pipeline` — no workflow hand-editing.

> The matrix workflows (scheduled health checks, credential rotation) use
> `llz env list --json` to fan out over **all** deployments at once; a promotion
> pipeline is the opposite shape — **sequential**, gated — so it is a `needs:`
> chain rather than a matrix. `llz env list --ordered` / `llz env next` expose the
> same ranks for scripting and documentation. The two coexist on one instance.

## 5. Per-stage differences are a feature, not a fork

Because each deployment owns its tfvars and overlay, stages can differ without
branching the repo:

| Knob | Where | Typical pipeline use |
|---|---|---|
| `apl_chart_version` | `cluster-bootstrap/<env>.tfvars` | bump `dev` first, promote the pin to `staging`/`prod` once green |
| `k8s_version` | `cluster/<env>.tfvars` | canary a new LKE-E version on `dev` |
| node sizing / count | `cluster/<env>.tfvars` | smaller `dev`, production-sized `prod` |
| region | `cluster/<env>.tfvars` | co-locate or spread stages |
| Helm values | `apl-values/<env>/values.yaml` | per-stage replicas, hostnames, feature flags |

Promotion of a *version* pin, then, is literally: edit `dev`'s pin → merge →
build `dev` → on green, copy the pin into `staging`'s tfvars in a follow-up PR →
build `staging`, and so on. `llz validate --env <env>` flags any unfilled
placeholder before each stage's build, so a half-configured stage fails fast.

## 6. Ordering caveats that interact with promotion

Two existing constraints layer on top of the promotion order — promotion does not
remove them:

- **Bootstrap-first sequencing.** The *first* cluster you ever bootstrap writes
  Harbor robot credentials that later clusters read; always bootstrap one fully
  before the next ([bootstrap-openbao.md](runbooks/bootstrap-openbao.md#additional-cluster-ordering-constraint)).
  For a fresh pipeline, bootstrap stages in `promotion_rank` order so this falls
  out naturally.
- **HA pairs are a different axis.** `promotion_rank` orders *stages*; `ha_role`/
  `ha_group` pair two clusters into one OpenBao HA topology
  ([secrets.md](secrets.md)). A stage can itself be an HA pair — rank one member
  (or both, with distinct ranks) as your topology requires. The two fields are
  independent: `llz env list --ha` and `llz env list --ordered` answer different
  questions.

## 7. What this is *not*

This is platform/infrastructure promotion — rolling an instance-repo change
across clusters in order. It is **not** application-level continuous delivery of
*your* workloads onto a cluster; that is apl-core / Argo CD's job, and the
"Deploy GitHub Environment" row in the [quickstart's environment table](quickstart.md#0-a-word-on-environments-first)
is the seam for app secrets. Keep the two mental models separate: `promotion_rank`
sequences *clusters*; Argo sequences *apps within* a cluster.

## See also

- [quickstart.md](quickstart.md) — single-deployment end-to-end path and the
  three meanings of "environment".
- [secrets.md](secrets.md) — OpenBao HA topology (`ha_role`/`ha_group`), the
  other per-deployment ordering axis.
- [runbooks/bootstrap-openbao.md](runbooks/bootstrap-openbao.md) — the
  bootstrap-first ordering constraint.
- `llz env list --help`, `llz env next --help`, `llz env add --help`.
