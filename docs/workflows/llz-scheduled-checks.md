# `llz-scheduled-checks` — maintainer rationale

`instance-template/.github/workflows/llz-scheduled-checks.yml` is the reusable
(`workflow_call`) body of the scheduled health/audit checks. It is **vendored
verbatim into every customer instance by copier**, alongside the composite
actions it calls, so each job runs self-contained with no cross-repo checkout —
which is what lets it run on an air-gapped GitHub Enterprise. An instance ships a
thin caller stub that owns the `schedule:` crons and passes secrets/inputs down;
`github.event_name` / `github.event.schedule` are inherited from the caller, so
every job's schedule/dispatch `if:` gate behaves exactly as it would in a
top-level workflow. See
`docs/adr/0003-vendor-actions-and-bodies-into-instances.md` for the
surface-reduction pattern.

Because the YAML is copied into instances where it can never be updated in
place, long-form maintainer archaeology — incident IDs, PR numbers, "we tried X
and it failed because…" — lives here in the template repo instead of in the
workflow body. This document is the archive; the inline comments are the 3am
debugging aids.

---

## Inputs and secrets

### `drift_branch`

Deliberately distinct from the instance's template pin: drift measures distance
to the *moving branch head*, not to the pinned release the instance was rendered
from. Comparing against the pin would always report zero drift.

### `APL_VALUES_REPO_TOKEN`

A github.com fine-grained PAT (Contents: write) used for the apl-core
`otomi.git` and the argocd repo Secrets. It is checked for expiry by the
`credential-single-pane` job's `token-inventory` step.

---

## Job: `discover`

Single source of truth for every per-env matrix in this workflow — see
`llz-discover-deployments.yml`. The credential rotation workflow calls the
**same** reusable workflow, so the set of deployments these checks verify cannot
drift from the set the rotation propagates into.
That coupling is the point: it makes "checked but unrotated" and "rotated but
unchecked" deployments structurally impossible rather than merely unlikely.

---

## Job: `weekly-cluster-checks`

### One job, three checks — why it was folded

This was previously **four separate jobs**: `openbao-health`,
`loki-objkey-rotation-health`, `certmanager-health`, and `wave-health-vap`. All
four sat on the same weekly cron, and each paid its own container init +
checkout + kubeconfig fetch + control-plane ACL open/revoke.

Three remain: `loki-objkey-rotation-health` was **retired** in #483 — see
[Retired: the Loki OBJ-key step](#retired-the-loki-obj-key-step) below.

Folding them into one job saves three of those cycles per region per week. The
part that matters more than runner minutes: it makes **three fewer
read-modify-write mutations of the shared LKE-E ACL object**, which the four
concurrent jobs were racing on every single week.

### No `continue-on-error`, anywhere — deliberately

This is a change from the four jobs that preceded this one, three of which
carried `continue-on-error` at job level.

That flag was swallowing less than it appeared to, and it was swallowing the
wrong thing. `health-openbao` and `health-certmanager` are warn-only **on a
finding** — they emit `::warning::` and exit 0 when they read the cluster and do
not like what they see. They still **fail when they cannot read it at all** (an
unreadable Certificate list is an error, not an empty one), which is the
fail-closed rule this repo applies everywhere: "could not tell" is never a pass.
So `continue-on-error` could only ever hide a **probe-path** failure: a dead
checkout, a failed ACL open, a bad image pull. That is precisely what this
weekly run exists to prove still works, and a probe that cannot report itself
broken re-proves nothing.

Two of the checks fail on a real FINDING — as distinct from failing because they could not look, which every check here does:

| Check | Fails when |
|---|---|
| `assert-wave-health-vap` | the wave-health guard VAP stopped enforcing — the PR #142 bootstrap-wedge class it exists to prevent |
| `assert-apl-deployed-version` | the apl-core RUNNING here is a MAJOR away from the version this llz release targets, or its version cannot be read at all. On managed App Platform **Linode** moves that version on their own schedule, with no event this repo sees, so a weekly cron is the only thing that will ever notice. Set the repo variable `LLZ_ALLOW_APL_CHART_MAJOR_DRIFT=1` to stage a major deliberately; a minor or patch apart only warns |

There were two. `health-loki-objkey` was the other, and it is gone (#483) — it
never once fired, which is the whole story below.

Every check carries `if: always()` (except the first, which has nothing before
it to skip on), so one failure does not skip its siblings — all still run and
report. Cleanup and ACL revoke are `if: always()` too.

### Step: Check OpenBao seal + ESO readiness

Checks each cluster for:

- OpenBao pod seal state (all 3 Raft pods)
- ESO `ClusterSecretStore` readiness
- Any unhealthy `ExternalSecrets` across all namespaces

Findings are warning-only **in the verb** — `health-openbao` emits `::warning::`
and exits 0, so a sealed pod pages via annotations rather than reddening the
run.

**Demoted to weekly (was daily).** The in-cluster `llz-reconciler` now samples
these continuously and fires them as Prometheus alerts —
`OpenBaoSealed` / `OpenBaoNoActiveLeader` (observability `openbao-alerts`) and
`LLZESOStoreNotReady` (`llz-reconciler` PrometheusRule) — so a daily
hosted-runner probe (fetch-kubeconfig + runner-ACL, the CIDR-fragile
port-forward path this migration set out to retire) is redundant. The weekly run
stays as belt-and-suspenders: it catches a cluster whose operator has not wired
an Alertmanager receiver (`spec.alerting.receivers: [none]`) and it re-proves
the probe path itself. Still available on demand via `workflow_dispatch`.

### Step: Check cert-manager Certificate readiness

Checks every cert-manager `Certificate` across all namespaces for `Ready=True`.
A stuck ACME renewal (e.g. a DNS-01 challenge failure for
`otel.<env>.internal`) leaves the Certificate in `Ready=False` indefinitely —
this surfaces it before pods start rejecting connections.

**Demoted to weekly (was daily).** The in-cluster `llz-reconciler` now samples
cert-manager Certificate readiness continuously (`llz_certificates_not_ready`)
and fires the `LLZCertificatesNotReady` Prometheus alert, so the daily
hosted-runner probe is redundant. Weekly stays as belt-and-suspenders (covers a
receiver-less cluster and re-proves the probe path). Still on demand via
`workflow_dispatch`.

### Step: Assert the wave-health guard VAP is bound and enforcing

Note the `tee -a`, not `tee`. As its own job this step truncated
`$GITHUB_STEP_SUMMARY`, which was harmless only because nothing else in that job
wrote to it. Sharing a job with three other checks makes that a live bug: it
would clobber whatever the steps above had written.

### Retired: the Loki OBJ-key step

This job used to end with the `health-loki-objkey-rotation` verb, labelled *"THE
GATE — deliberately no `continue-on-error`, and deliberately last."* It was
removed in #483. **Do not add it back**; read this first if you are tempted.

**It could not fire.** The check measured `secret/loki/object-store` by
`kubectl exec`-ing `bao kv metadata get` inside the OpenBao pod with
`OPENBAO_ROOT_TOKEN`. That token is *expected absent*: bootstrap revokes it, and
this workflow declared it `required: false` and said so in its own comment.
Worse, this job never mapped the secret into the step's environment at all — so
even an instance that had left a root token parked would have measured nothing.
Every run took the no-token branch, warned, and exited 0. A gate labelled as a
gate, green for its entire life, on a credential nobody was checking.

**The measurement did not go with it.** Three mechanisms cover the platform's
object-storage credential today, none of them needing a root token.

The credential itself has changed since #483: the check measured
`secret/loki/object-store`, a per-app key whose ExternalSecret was deleted when
object storage went apl-core-native. That path has since been retired outright —
it had no consumer — and `secret/obj/platform` is the key apl-core actually reads,
for Loki and Harbor both. So the coverage below is about the credential that
matters rather than the one #483 happened to name:

| Mechanism | Where it runs | Threshold | Cadence |
|---|---|---|---|
| `llz_credential_age_days{cred="obj-platform"}` | `llz-reconciler`, over Kubernetes auth | — (a gauge) | every sample pass |
| `LLZCredentialRotationOverdue` | in-cluster alert | > 90d | continuous |
| `llz ci assert-rotation-health` | the credential-single-pane job below | > 90d | **daily** |

Every column is a tightening: 90 days rather than 120, daily rather than weekly,
and `assert-rotation-health` fails on an **absent series** as well as an overdue
age — which an exec that never ran could not do at all.

**What holds the replacement to it.**
`TestTheObjectStoreKeyIsGatedByTheAgeLane`
(`tools/internal/extensions/assertions/assertsecrets/ci_assert_rotation_health_test.go`)
feeds the real
`credpaths.CredPaths` declaration into the real `evalRotationHealth` predicate
and fails if this credential is dropped from the table, marked `Optional`, or
demoted to a class the gate only reports on. Any of those would silently return
coverage to the nothing the retired step provided.

**The 120-day number is gone too.** Two mechanisms claiming different SLAs for
one credential is worse than either number; 90 is the stricter, it is what the
alert has always used, and `TestRotationSLAsMatchThePrometheusRules` pins the
gate to it.

---

## Job: `app-scope-health`

The other half of the convergence boundary, and the only blocking gate the
instance-owned estate has.

`llz ci converge` gates the **platform**. Instance-owned content — the operator
escape hatch and the apps deployed through it — is reported by that run and
excluded from its verdict, so an app team's unseeded credential cannot block a
platform release. That exclusion is only safe if something else says no. This
job is that something: `llz ci converge --scope=apps`, weekly per region, with
**no `continue-on-error`**.

### Why a job and not a step

It shipped first as a warn-only step inside `weekly-cluster-checks`, which made
it the one check in that job that could never go red — the boundary removed app
content from the platform gate and handed it a check that reported and passed.
That is the failure mode the boundary was drawn to fix, not a smaller version of
it: on akamai/gsap-apl eight per-app PATs went unseeded for eight days because
nothing red ever appeared.

It cannot be a *blocking* step there either. The steps in that job carry
`if: always()` precisely because a red step otherwise skips its siblings, and a
blocking app-scope step would put the platform's OpenBao and cert-manager probes
downstream of an app team's deploy — the coupling this boundary exists to
remove, re-created one layer up. A separate job has its own verdict, its own
`cluster-access`, its own owner, and can skip nothing.

### Why not `llz-cluster-health`

That workflow is `workflow_dispatch`-only. A gate placed there runs when a human
clicks it, which is not a gate.

### Why `converge` and not `health`

`health.Budgeted` is true only inside a budget. Outside one, a pod that is merely
being created classifies as **failed**, so a one-shot `llz ci health --scope=apps`
reports false hard failures on a routine app rollout. Every operator instruction
to inspect the app scope should say `converge --scope=apps`; `health --scope=apps`
exists for a caller that already knows the estate is settled.

The budget is 300s rather than the bootstrap gate's 1200s — this is a
steady-state sweep, not a wait for a cluster that is still coming up.

### What it does NOT replace

The continuous detector is the in-cluster `LLZAppScopeNotConverged` alert, which
fires within the hour. That alert is **Application-level**: it classifies Argo
Applications, so a `Synced`+`Healthy` app whose ExternalSecrets, Jobs or Pods are
broken does not move its gauge. This job is what sees the resource level, and it
sees it weekly. See [reconciler-alerts.md](../runbooks/reconciler-alerts.md).

---

## Job: `lke-admin-rotation-health`

### Step: Cluster access (kubeconfig + runner ACL + llz)

Fetches the kubeconfig, opens this hosted runner's dynamic egress IP in the
LKE-E control-plane ACL so the `kubectl` checks below are permitted (revoked at
job end), and installs `llz`. `allow-missing: true` — these checks tolerate a
torn-down cluster: the ACL open and the `llz` install are skipped internally,
and the check steps gate on the `available` output.

### Step: Check lke-admin-token age

A native port of the former shell implementation. Newest-token age versus the
warn/critical SLA is `health.ClassifyRotationAge`; the cluster-unreachable skip
lives in the command rather than in workflow YAML.

---

## Job: `credential-single-pane`

Runs daily. It replaces the former per-provider probe jobs (the Linode
`cred-audit` and `gh-pat-expiry-health`) with a cluster-as-source-of-truth flow:
two steps sharing one kubeconfig.

It runs per-region because Linode PATs are per-env while GitHub PATs are
instance-wide, so each cluster's reconciler exposes that region's view. It
self-skips a not-bootstrapped deployment via `cluster-access`'s `allow-missing`
plus the writer's own token skip.

### Step: Write token inventory to the cluster (WRITER)

Measures the expiry of every CI token this job holds — GitHub service PATs (via
the `token-expiration` response header) and Linode account PATs (via the API) —
and writes the `llz-token-inventory` ConfigMap that the in-cluster reconciler
re-exposes as `llz_token_expiry_*` metrics. **Only metadata leaves; never a
token.**

`kubectl` defaults to the `$HOME/.kube/config` that the `cluster-access` step
wrote, and the default GitHub shell (`-eo pipefail`) fails the step if either
side of the pipe does.

### Step: Read cluster credential alert status (READER)

Asks **live** Prometheus whether any credential alert (`LLZToken*` /
`LLZCertificate*`) is firing or BROKEN. The cluster is the single pane of glass;
the actual expiry pages via Alertmanager.

`--strict` fails the job if a credential alert cannot evaluate — a missing
metric, e.g. because the funnel is down. This catches a broken pipeline that the
per-provider probes used to mask: they would happily report "no findings" while
the thing that produces findings was dead. `--summary` writes the verdict table
to the job summary (see `llz ci alert-eval`).

---

## Job: `prometheusrule-health`

### Step: Cluster access (kubeconfig + runner ACL)

Fetches the kubeconfig and opens this hosted runner's dynamic egress IP in the
LKE-E control-plane ACL so the `kubectl` checks below are permitted (revoked at
job end). `allow-missing` — these checks tolerate a torn-down cluster: the ACL
open is skipped internally and the check steps gate on `available`.

### Step: Check Prometheus has loaded rules (no evaluation errors)

`health-prom-rules` queries Prometheus `/api/v1/rules` and reports any rule group
with `lastError` set — evaluation failures that promtool's syntax check cannot
catch.

Rule findings are annotations, but an **unreachable Prometheus fails the job**:
the check cannot report on rules it never read, and the job no longer carries
`continue-on-error` to swallow that.

The original implementation looked for the Prometheus pod in
`llz-observability`, which holds the LLZ CRs — while apl-core Prometheus
actually runs in `monitoring`. So it skipped clean on every run and nothing
validated the live rules.

---

## Job: `template-drift`

Deliberately a **separate job**, not a step in another: it runs on its own
monthly cron (`0 7 1 * *`) rather than the daily/weekly ones, and it needs no
`environment:` — it touches no cluster and no per-region secrets, only the
instance checkout and github.com.

### Step: Compare the instance's template pin against the template head

The instance is checked out at the workspace root; the pin recorded in its
`.copier-answers.yml` is what gets compared.

`llz drift` resolves the template head via `git ls-remote`, which needs auth when
the template repo is private — hence the `GH_TOKEN` env var and the
`git config --global url.<...>.insteadOf` rewrite that routes github.com fetches
through the token.

---

## Removed jobs

### `go-vuln-audit` (removed)

Ran `govulncheck` over the template's Go tools module. It was removed from this
workflow for two reasons:

1. It checked out the **central template repo**, which a self-contained instance
   cannot reach on an air-gapped GitHub Enterprise.
2. It audited **template** code rather than instance config, so it did not
   belong in a per-instance scheduled check at all.

CVE-auditing the tools module belongs in the template repo's own CI — see
`docs/designs/cross-org-reuse-pattern.md`.
