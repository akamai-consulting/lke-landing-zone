# Argo CD Operations — Playbook

**Applies to:** the Argo CD instance reconciling every Application for your platform workloads on each regional cluster.

**Related:** [`operator-onboarding.md`](operator-onboarding.md), `llz status <env>` (one-shot support-plane Application health report; `--wait` polls), and the sync-wave + correctness rules described in [`docs/architecture/convergence-contract.md`](../architecture/convergence-contract.md).

> **Rule of thumb:** the change you want to make is almost always a PR to the Argo manifests (the shared platform tree your `apl-values/<env>/manifest` overlay remote-refs) that Argo CD reconciles. `kubectl edit` and the Argo CD UI's manual-sync button are for unwedging a stuck reconciliation, not for routine changes. Direct edits get blown away on next sync.

---

<!-- toc -->
## Contents

- [Get into Argo CD](#get-into-argo-cd)
- [Day-2 cheatsheet](#day-2-cheatsheet)
- [Common situations](#common-situations)
- [Self-healing & auto-sync — when to enable each](#self-healing--auto-sync--when-to-enable-each)
- [Reconciliation triggers + cadence](#reconciliation-triggers--cadence)
- [When Argo CD itself is broken](#when-argo-cd-itself-is-broken)

<!-- /toc -->

## Get into Argo CD

Argo CD is installed and owned by apl-core, and is **Keycloak-SSO'd** — the same
identity you use for `llz openbao login`. The platform console links to it.

```bash
# Per-region (each cluster runs its own Argo CD)
kubectl -n argocd port-forward svc/argocd-server 8080:443
# Browse https://localhost:8080 and use the SSO button.
```

> **The upstream chart's local admin may not exist here.** `argocd-initial-admin-secret`
> is created by the community argo-cd chart; apl-core's Argo CD is SSO-integrated and
> nothing in LLZ references that Secret. Check before you plan around it:
>
> ```bash
> kubectl -n argocd get secret argocd-initial-admin-secret   # NotFound is expected
> ```
>
> If it does exist and you need the local admin for a break-glass case:
> `kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d`

For the CLI, log in through SSO:

```bash
argocd login localhost:8080 --insecure --sso
```

Install the `argocd` CLI yourself — `brew install argocd` on macOS, or the release
binary. (Your instance has no Makefile; the [Dev Container](../devcontainer.md)
ships it.)

---

## Day-2 cheatsheet

```bash
# What's the state of every Application?
kubectl -n argocd get applications
argocd app list                          # same, but with sync/health columns

# One-shot support-plane health report (useful for incident triage);
# add --wait to poll until the required Applications converge.
llz status <env>

# Drill into one app
argocd app get <app-name>                # shows status, conditions, resources
kubectl -n argocd get application <app-name> -o yaml | yq .status

# Force a refresh (re-pull manifests from git; doesn't apply changes)
argocd app get <app-name> --refresh

# Force a sync (apply current desired state to the cluster)
argocd app sync <app-name>

# Force a hard sync (re-create resources that have drifted)
argocd app sync <app-name> --force --replace

# Watch a sync in real time
argocd app sync <app-name> --watch
```

---

## Common situations

### App stuck `OutOfSync`

Two causes dominate here, and the second is the one that wastes a day.

**1. Something changed the resource in-cluster** (a person, or another controller)
and the app has `selfHeal: false` or no auto-sync, so Argo CD will not correct it.

**2. Nothing changed it — Argo is comparing the wrong thing.** Argo's default
client-side diff compares your manifest against the live object *including fields
the cluster wrote itself*: CRD defaults filled in by ESO/Kyverno, an admission
webhook's mutation, a `mutateDigest`-rewritten image. Those never appear in git, so
the app is permanently `OutOfSync` with a diff you cannot fix by editing anything.

Every LLZ-carved Application already ships
`argocd.argoproj.io/compare-options: ServerSideDiff=true,IncludeMutationWebhook=true`
for this reason. If you see permanent drift on an app **without** that annotation,
that is the first thing to check.

Settle which one you are looking at before changing anything — ask the server what
it would actually write:

```bash
kubectl apply --server-side --dry-run=server -f <the-manifest-argo-syncs>
```

If the result matches the live object, the manifest is fine and the diff is
client-side — add the compare-options annotation rather than editing the manifest.

> **Do not "fix" this with `ignoreDifferences` on an image path.** It silences the
> diff and silently stops image bumps from ever rolling out.

```bash
# 1. See exactly what differs
argocd app diff <app-name>

# 2. If the in-cluster state is wrong: re-apply the git state
argocd app sync <app-name>

# 3. If the git state is wrong: fix the YAML in a PR, let it merge, sync.
```

Don't `kubectl edit` to "fix" the drift — that just resets the clock until Argo CD next reconciles or someone re-syncs.

### App stuck `Degraded` (`Healthy: false`)

Means the resources Argo CD applied are unhealthy — Pod CrashLoopBackOff, Deployment progressing too long, ExternalSecret SecretSyncedError, etc.

```bash
# 1. See which resource is degraded
argocd app get <app-name>          # lists every resource with health
kubectl -n argocd get application <app-name> -o jsonpath='{.status.resources}' | jq

# 2. Investigate the specific resource
kubectl describe <kind> <name> -n <ns>
kubectl logs -n <ns> <pod-name>
kubectl -n <ns> get events --sort-by='.lastTimestamp' | tail -20

# 3. For ExternalSecrets specifically:
#    SecretSyncedError → OpenBao path missing or ESO can't auth
kubectl describe externalsecret <name> -n <ns>
# Compare spec.data[].remoteRef.key against the OpenBao paths `llz ci
# bao-configure` grants — anything new must be in the read-only `platform-ci`
# policy (ESO's Kubernetes-auth role), or under a declared team subtree.
```

### Sync hangs forever

If `argocd app sync` doesn't return and the UI shows the sync waiting on a resource:

```bash
# 1. Check the resource the sync is blocked on (UI: sync operations tab)
#    Common culprits: jobs that don't terminate, hooks that wait on a missing dep.

# 2. Terminate the sync
argocd app terminate-op <app-name>

# 3. Fix the underlying resource, then re-sync.
```

### `ComparisonError` / `Unknown` health

Means Argo CD couldn't render the manifests (Helm/Kustomize error) or compare them to the cluster.

```bash
argocd app get <app-name>                            # see the error message
argocd app manifests <app-name> --source live        # what's deployed
argocd app manifests <app-name> --source git         # what should be deployed
```

If the error is a Helm template failure, reproduce locally with `helm template` + the same values to debug — the repo's Helm-lint target catches most of these before they ship.

### A throwing health check deadlocks the app against its own fix

The variant that wastes the most time, because **merging the fix does nothing**.

Argo evaluates a custom (Lua) health check for many resource kinds. If that check
*throws* rather than returning a verdict — typically on a half-created resource
whose status fields are `nil`, e.g. `cannot perform concat operation between string
and nil` — the error aborts the **whole Application's comparison**. And `selfHeal`
only ever runs *after* a successful comparison. So the app sits `OutOfSync` at the
old revision, and the corrected manifest you just merged is never applied, because
the broken resource prevents Argo from getting far enough to apply it.

The tell is an app that stays `OutOfSync`/`Unknown` across several merges with a
`ComparisonError` naming a Lua/health error rather than a render error.

Break the loop by converging the **live** resource to what git already says, so its
health check stops throwing and comparison can complete:

```bash
# 1. Patch the live resource to match the merged git value.
kubectl patch <kind>/<name> --type=merge -p '{"spec":{...}}'   # the corrected value from git
# 2. Watch it reach a state the health check can evaluate; the ComparisonError clears.
kubectl get <kind>/<name> -w
# 3. A sync that already terminated Failed does not auto-retry — kick one manual sync.
kubectl -n argocd patch application <app> --type merge \
  -p '{"operation":{"sync":{"revision":"HEAD","syncStrategy":{"apply":{}}}}}'
```

The patch is **convergent** — it matches git — so `selfHeal` will not fight it, and
step 3 is a one-off rather than a new standing workaround.

Observed downstream (gsap-apl) with a Crossplane `Provider` pointing at a package
that 404s, but nothing about it is Crossplane-specific: any CRD with a custom health
check can do this.

### A path seeded after the store is Ready waits out ESO's backoff

An `ExternalSecret` that has **never** synced retries with exponential backoff
capping near **~16 minutes** — `refreshInterval` only applies *after* the first
successful sync. So when you seed a missing OpenBao path by hand, the Secret does
not appear promptly; it appears whenever the backoff next fires, and until then the
consumer sits in `CreateContainerConfigError` or the CR in `ReconcileError`.

The `llz-reconciler` es-store-recovery lane does **not** cover this case: it fires
on the `openbao` ClusterSecretStore's Ready `False→True` transition, and here the
store was healthy all along — only the *path* was missing. Nothing transitions, so
nothing nudges.

After seeding a path late, force the one reconcile rather than waiting:

```bash
kubectl -n <ns> annotate externalsecret <name> force-sync="$(date +%s)" --overwrite
```

This is an operator action during bring-up, not a pattern to automate: a controller
that bumps `force-sync` on a timer is [anti-pattern #6](../designs/kube-native-reconciler.md).
If you find yourself running it routinely, the real fix is seeding the path *before*
the consumer syncs — see [secrets-before-apps](../designs/secrets-before-apps.md).

### AppProject missing / sync-wave violation

Every `Application` and `AppProject` must carry `argocd.argoproj.io/sync-wave: "N"` (see the sync-wave + correctness rules in [`docs/architecture/convergence-contract.md`](../architecture/convergence-contract.md)). If a new manifest fails CI with a `llz ci argocd-rendered-apps` (which absorbed the former `sync-wave-lint`) error:

- Add the annotation per the wave table in the convergence contract.
- AppProjects: wave `-20`. Applications: usually `0` or higher; cert-manager bootstrap and CRD-installing apps go earlier.

### Force-recreate an app from scratch

When sync drift is too tangled to untangle in place:

```bash
# 1. Delete the Application (NOT the resources — propagationPolicy=orphan)
argocd app delete <app-name> --cascade=false

# 2. Re-create from git (just merge a no-op PR or run argocd app create against the manifest)

# 3. Sync the new Application
argocd app sync <app-name>
```

If the resources themselves are corrupt, drop `--cascade=false` and Argo CD cleans them up too — but think hard before deleting an Application that owns persistent state (Loki object storage, Harbor projects, etc.).

---

## Self-healing & auto-sync — when to enable each

In this setup most apps are `automated: true, selfHeal: true` (e.g. `cert-manager`, `external-secrets`, `observability/*`). One intentional exception:

- **`firewall-controller`** — manual sync only (the Application manifest carries an enable-on-demand comment). Reason: the controller mutates Linode Cloud Firewall state; auto-syncing during an incident can re-apply a broken rule set faster than you can stop it.

  > **Akamai-internal.** This Application ships from the private
  > `lke-landing-zone-internal` repo, not the public template — if you do not see it
  > in your cluster, that is why. The public template ships the `cidrFirewall`
  > component instead.

If you're tempted to flip `selfHeal: true` somewhere it's currently off, ask why it's off first — usually a deliberate operator-gate-required reason.

---

## Reconciliation triggers + cadence

- Polling: every 3 minutes (the default; no override in our values).
- Webhook from `github.com`: not currently wired. PRs reconcile within the polling window.
- Manual: `argocd app sync` or the UI button.

If you want a change applied *right now* after merging a PR, run `argocd app sync <name> --watch` from your laptop. Otherwise wait ~3 minutes.

---

## When Argo CD itself is broken

Argo CD is just another Application managed by `kubectl` directly — if its server pods are down, you can't use the CLI. Recovery:

```bash
# 1. Check the argocd namespace
kubectl -n argocd get pods
kubectl -n argocd describe pod <argocd-server-...>

# 2. Common causes:
#    - LKE node restart: argocd-repo-server has cached chart deps that vanished.
#      → kubectl -n argocd rollout restart deployment/argocd-repo-server
#    - Repo credential rotated: argocd-server can't pull from github.com.
#      → the credential is apl-core's argocd-repo-creds ExternalSecret, backed by
#        the APL_VALUES_REPO_TOKEN PAT over HTTPS (the old ARGOCD_REPO_SSH_KEY
#        deploy key was retired). Check the ExternalSecret synced, then restart.

# 3. Argo CD itself is installed by apl-core, not by a manifest in this repo.
#    To re-create it, re-run the bootstrap (`llz ci bootstrap-cluster`).
```

In a true Argo-CD-down emergency, fall back to `kubectl apply -f` against the Argo manifests directly — but expect Argo CD to undo any drift the moment it comes back up unless you also fixed the source.
