# Moving `credential-single-pane` in-cluster — analysis + open decisions

Status: **analysis / not yet implemented.** This is the input to the coordinated
credential-rotation / PAT-window review (the same review that owns the item-3
credential-SLA audits). It exists because the migration looked like the low-risk
"kube-only, just delete the ACL dance" case, and the code says otherwise: it needs
two security-posture changes the platform currently guards against, which belong
in a deliberate InfoSec sign-off rather than a drive-by PR.

See also: [credential-single-pane.md](credential-single-pane.md) (the feature),
[kube-native-reconciler.md](kube-native-reconciler.md) (what the reconciler retires),
[broadPatRotator](../../platform-apl/components/broadPatRotator/) (the in-cluster
external-credential substrate this would copy).

## What runs today (out of cluster)

The `credential-single-pane` job in
[`.github/workflows/llz-scheduled-checks.yml`](../../.github/workflows/llz-scheduled-checks.yml)
runs **daily, per region** (matrix). Per run it:

1. **Opens the ACL dance** — the `cluster-access` composite action reads the cluster
   kubeconfig out of Terraform state (S3 backend) and opens the LKE-Enterprise
   control-plane ACL for the hosted runner's dynamic egress IP (`llz ci runner-acl open`).
2. **Writes the inventory** — `llz ci token-inventory | kubectl apply -f -`
   ([`ci_token_inventory.go`](../../tools/cmd/llz/ci_token_inventory.go)) measures the
   expiry of every CI token it holds — two GitHub service PATs
   (`OPENBAO_SECRETS_WRITE_TOKEN`, `APL_VALUES_REPO_TOKEN`) via the token-expiration
   header, plus Linode account PATs via `GET /v4/profile/tokens` — and applies the
   `llz-reconciler/llz-token-inventory` ConfigMap (metadata only, never a token value).
3. **Evaluates the alerts** — `llz ci alert-eval --match '^LLZ(Token|Certificate|Credential)'
   --strict` asks live Prometheus whether any credential alert is FIRING/DEAD?/BROKEN.
4. **Closes the ACL dance** — deletes the kubeconfig and revokes the runner ACL.

The in-cluster `llz-reconciler` already reads that ConfigMap every 60s
([`reconcile_tokens.go`](../../tools/cmd/llz/reconcile_tokens.go)) and re-exposes it as
`llz_token_expiry_timestamp_seconds{provider,token}` etc., so Prometheus + Alertmanager
already carry the *alerting*. The out-of-cluster job is only the **writer** (step 2) plus
a **CI gate** (step 3).

## The intended win

Run the writer in-cluster and the whole ACL dance (steps 1 + 4) disappears: an
in-cluster CronJob writes the ConfigMap directly through the apiserver, and its
external tokens move from GitHub Actions secrets to ESO-mounted OpenBao secrets —
the exact posture of [`broadPatRotator`](../../platform-apl/components/broadPatRotator/):
dedicated namespace, dedicated SA, default-deny + port-restricted egress NetworkPolicy,
ESO `ExternalSecret`s, CronJob on the slim distroless `llz` image. This retires the
CIDR-fragile port-forward path the reconciler migration set out to eliminate.

Two mechanical pieces are ready and uncontroversial:

- **`llz ci token-inventory --apply`.** The distroless image has no `kubectl`, so the
  `| kubectl apply` pipe can't run in-pod. The [`kube.Client`](../../tools/internal/kube/kube.go)
  already has `GetJSON` / `CreateJSON` / `MergePatch` — enough for a create-or-update of
  the ConfigMap (GET → 404 create, else merge-patch `data`), seamed for a unit test the
  same way the existing measurement path is. Pure-additive; the stdout path (and the CI
  job) keep working unchanged.
- **The component + cross-namespace RBAC.** The reconciler reads the ConfigMap from its
  *own* namespace (`reconcile_tokens.go` uses `podNamespace()` → `llz-reconciler`), so the
  writer must target `llz-reconciler`. Keeping the writer isolated in its own namespace
  (it holds broad tokens — see below) means a small Role/RoleBinding in `llz-reconciler`
  granting the writer SA `create`/`get`/`update`/`patch` on that one ConfigMap. Standard.

## Why it stops here — two security-posture decisions

Both are exactly what makes the item-3 credential-SLA work need a coordinated
PAT-window / InfoSec review, so item 1 is not a separable low-risk case; it collapses
into that review.

### 1. `APL_VALUES_REPO_TOKEN` has no home in OpenBao

It is one of the two GitHub PATs the inventory measures, but today it exists **only** as
a GitHub Actions secret. Its lifecycle: TF var `apl_values_repo_token`
([`cluster-bootstrap/variables.tf`](../../tools/internal/tfroots/roots/cluster-bootstrap/variables.tf))
→ apl-core's `otomi.git.password`. It is **not** in landing-zone OpenBao, not in any
`ExternalSecret`, and not in the `platform-ci` policy the ESO `openbao` store reads with
(that policy is a deliberate per-path allowlist — `ci_openbao_configure.go`, "wildcard
read is intentionally avoided").

To measure its expiry in-cluster you must **seed a GitHub PAT into the platform vault for
the first time**: add a `bao-seed` entry (e.g. `secret/infra/apl-values-repo-token`), add
`secret/data` + `secret/metadata` read lines to `policyPlatformCI`, and add an
`ExternalSecret`. The seed runs at bootstrap **fleet-wide**, so this is *not* inert the way
a default-disabled component is — it changes every instance's vault contents. Alternative:
drop `APL_VALUES_REPO_TOKEN` from the in-cluster pane and lose lead-time on the PAT whose
expiry breaks Argo/apl-core git sync — a real coverage regression.

### 2. The Linode half forces the broad `account:read_write` token per-cluster

`token-inventory` enumerates account PATs via `GET /v4/profile/tokens`. The narrow
in-cluster reconciler token (`secret/linode/api-token`) is minted with
`domains/object_storage/volumes/linodes/vpc/firewall` scopes and **no `account` scope**
(`ci_incluster_pat.go`); it is unproven against that endpoint, and every existing caller
(`cred-audit`, `rotate-broad-pat`, the out-of-cluster job) uses the **broad** token
(`secret/linode/broad-pat`, `account:read_write`). So a faithful port mounts the broad
token in a **standing per-cluster Secret**.

The design deliberately isolates that broad token to the single `broadPatRotator`
workload (its namespace/rbac comments are explicit) to keep its blast radius minimal.
Spreading it to a per-cluster inventory CronJob is a posture change InfoSec should weigh.
Cleaner-but-more-work alternative: confirm with Linode whether `GET /v4/profile/tokens`
needs `account` scope; if not, use the narrow token; if so, mint a new minimally-scoped
read-only path for this job.

## `alert-eval` (the CI gate) does not come along

`alert-eval` and its `prom_query.go` helper are built on `kubectl get prometheusrules`
plus a `kubectl port-forward` to Prometheus (the LKE-E apiserver `services/proxy`
subresource is webhook-denied, so port-forward is the only out-of-cluster path). Neither
`kubectl` exists on the distroless image; porting it would be a client-go + direct-HTTP
rewrite. It also isn't needed at runtime in-cluster: the reconciler already emits the
gauges continuously, the `LLZToken*`/`LLZCertificate*` rules already fire via Alertmanager,
and `LLZTokenInventoryStale` already covers a dead writer/funnel. `alert-eval` was a CI
gate, not a runtime dependency — when the job is eventually retired it is simply dropped.

## Sequencing note — retire the GHA job *after* adoption, not with the port

The writer feeds every cluster's pane. If the in-cluster component ships default-disabled
(the correct posture — it needs per-env secrets, like `broadPatRotator` /
`clusterHealthWorkflow`) while the GHA job is deleted in the same change, the credential
pane goes dark on every existing instance until each one opts in and seeds secrets. The
GHA `credential-single-pane` job must stay as the full-fidelity writer until an instance
enables the component. This is the `broadPatRotator` activation model.

## Item-2 note — the weekly health probes are demote-not-delete, on purpose

Related cleanup considered alongside this: deleting the weekly `openbao-health` +
cluster-health probes in `llz-scheduled-checks.yml`. Leaving them **as-is** is the correct
call. [`kube-native-reconciler.md`](kube-native-reconciler.md) is explicit that these were
*"demoted to belt-and-suspenders, **not deleted**, per the alerting-doc layering,"* kept to
catch *"a cluster whose operator has not wired an Alertmanager receiver"* and to re-prove
the probe path. Deleting them removes the only credential-health signal for any instance
without Alertmanager receivers wired. Keep them.

## Touch-points for when this proceeds

- `tools/cmd/llz/ci_token_inventory.go` (+ `_test.go`) — add `--apply` (create-or-update via
  `kube.Client`).
- `platform-apl/components/tokenInventory/` — new component (namespace, SA, cross-ns RBAC
  into `llz-reconciler`, `ExternalSecret`s, default-deny + egress NetworkPolicy, CronJob),
  modeled on `platform-apl/components/broadPatRotator/`.
- `tools/internal/clusterspec/components.go` — register `tokenInventory` (DependsOn
  `externalSecrets` + `llzReconciler`, `DefaultDisabled`, CarvedApp wave 5).
- `tools/cmd/llz/ci_bao_seed_all.go` + `ci_openbao_configure.go` (`policyPlatformCI`) — only
  if decision (1) is to vault `APL_VALUES_REPO_TOKEN`.
- `.github/workflows/llz-scheduled-checks.yml` — retire the `credential-single-pane` job
  **only after** per-instance adoption.
- `docs/designs/credential-single-pane.md` — update the data-flow (writer moves in-cluster)
  and code touch-points.
