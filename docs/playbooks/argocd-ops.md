# Argo CD Operations — Playbook

**Applies to:** the Argo CD instance reconciling every Application for your platform workloads on each regional cluster.

**Related:** [`operator-onboarding.md`](operator-onboarding.md), `llz status <env>` (one-shot support-plane Application health report; `--wait` polls), and the sync-wave + correctness rules described in [`docs/architecture/convergence-contract.md`](../architecture/convergence-contract.md).

> **Rule of thumb:** the change you want to make is almost always a PR to the Argo manifests (under `instance-template/apl-values/example/manifest/`) that Argo CD reconciles. `kubectl edit` and the Argo CD UI's manual-sync button are for unwedging a stuck reconciliation, not for routine changes. Direct edits get blown away on next sync.

---

## Get into Argo CD

```bash
# Per-region (each cluster runs its own Argo CD)
kubectl -n argocd port-forward svc/argocd-server 8080:443

# Browse: https://localhost:8080
# Default username: admin
# Initial admin password:
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d

# CLI alternative — most operations are simpler from the CLI:
argocd login localhost:8080 \
  --insecure \
  --username admin \
  --password "$(kubectl -n argocd get secret argocd-initial-admin-secret \
                  -o jsonpath='{.data.password}' | base64 -d)"
```

The argocd CLI is installed by the repo's tooling target; if you don't have it, `brew install argocd` works on macOS.

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

Most common cause: someone modified a resource in-cluster (or another controller patched it) and Argo CD won't auto-correct because the app has `selfHeal: false` or no auto-sync.

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
# Compare the spec.data[].remoteRef.key against the OpenBao paths in
# `llz ci bao-configure` (tools/cmd/llz/ci_openbao_configure.go) — anything new
# must be in the CI AppRole (platform-ci) policy.
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

### AppProject missing / sync-wave violation

Every `Application` and `AppProject` must carry `argocd.argoproj.io/sync-wave: "N"` (see the sync-wave + correctness rules in [`docs/architecture/convergence-contract.md`](../architecture/convergence-contract.md)). If a new manifest fails CI with a `sync-wave-lint` error:

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
#    - SSH deploy key rotated: argocd-server can't pull from github.com.
#      → re-seed the deploy-key secret and restart.

# 3. If you need to re-create Argo CD from scratch, the install manifest lives under
#    instance-template/apl-values/example/manifest/openbao/ — the bootstrap workflow can re-apply it.
```

In a true Argo-CD-down emergency, fall back to `kubectl apply -f` against the Argo manifests directly — but expect Argo CD to undo any drift the moment it comes back up unless you also fixed the source.
