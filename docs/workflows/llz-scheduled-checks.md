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

### `template-ref`

The template release the instance is rendered from — `llz upgrade` re-pins it.
It is **unused by this workflow's jobs** (everything resolves locally, from the
vendored copy). It is declared only because the caller stub passes it and
`workflow_call` rejects undeclared inputs.

### `drift_branch`

Deliberately distinct from `template-ref`: drift measures distance to the
*moving branch head*, not to the pinned release the instance was rendered from.
Comparing against `template-ref` would always report zero drift.

### `APL_VALUES_REPO_TOKEN`

A github.com fine-grained PAT (Contents: write) used for the apl-core
`otomi.git` and the argocd repo Secrets. It is checked for expiry by the
`credential-single-pane` job's `token-inventory` step.

---

## Job: `discover`

Single source of truth for every per-env matrix in this workflow — see
`llz-discover-deployments.yml`. The credential rotation workflow and the
Terraform PR plan call the **same** reusable workflow, so the set of deployments
these checks verify cannot drift from the set the rotation propagates into.
That coupling is the point: it makes "checked but unrotated" and "rotated but
unchecked" deployments structurally impossible rather than merely unlikely.

---

## Job: `weekly-cluster-checks`

### One job, four checks — why it was folded

This was previously **four separate jobs**: `openbao-health`,
`loki-objkey-rotation-health`, `certmanager-health`, and `wave-health-vap`. All
four sat on the same weekly cron, and each paid its own container init +
checkout + kubeconfig fetch + control-plane ACL open/revoke.

Folding them into one job saves three of those cycles per region per week. The
part that matters more than runner minutes: it makes **three fewer
read-modify-write mutations of the shared LKE-E ACL object**, which the four
concurrent jobs were racing on every single week.

### No `continue-on-error`, anywhere — deliberately

This is a change from the four jobs that preceded this one, three of which
carried `continue-on-error` at job level.

That flag was swallowing less than it appeared to, and it was swallowing the
wrong thing. `health-openbao` and `health-certmanager` are warn-only **in Go**
("never fails the job") — they emit `::warning::` and exit 0 on a finding. So
`continue-on-error` could only ever hide a **probe-path** failure: a dead
checkout, a failed ACL open, a bad image pull. That is precisely what this
weekly run exists to prove still works, and a probe that cannot report itself
broken re-proves nothing.

Two of the checks *do* fail on a real finding, and now say so:

| Check | Fails when |
|---|---|
| `assert-wave-health-vap` | the wave-health guard VAP stopped enforcing — the PR #142 bootstrap-wedge class it exists to prevent |
| `health-loki-objkey` | the object-store key breached its rotation SLA |

Every check carries `if: always()` (except the first, which has nothing before
it to skip on), so one failure does not skip its siblings — all four still run
and report. Cleanup and ACL revoke are `if: always()` too.

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

### Step: Check secret/loki/object-store age

**The gate** — deliberately no `continue-on-error`, and deliberately last.

Linode Object Storage keys have no native expiry, and the Guidelines mandate
revoking bucket access keys after 120 days. The object-storage Terraform module
force-rotates the key declaratively (`time_rotating`), but the OpenBao reseed
hop is manual — this check reads the age of the current
`secret/loki/object-store` version and alerts if the live credential has fallen
behind the 120-day clock.

**Demoted to weekly (was daily).** The in-cluster `llz-reconciler` samples the
same OpenBao rotation age continuously (`llz_credential_age_days{cred=...}`) and
fires `LLZCredentialRotationOverdue` (>90d) for both the Loki and Harbor
object-storage keys, so the daily hosted-runner probe is redundant. Weekly stays
as belt-and-suspenders. Still on demand via `workflow_dispatch`.

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

### Step: Compare .template-version against the template head

The instance is checked out at the workspace root; its `.template-version` is
what gets compared.

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
