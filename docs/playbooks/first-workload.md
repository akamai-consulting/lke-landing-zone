# Run your first workload — Playbook

**Applies to:** an instance whose platform has converged (`llz status <env>` green).
This is the step *after* the platform: getting your own application running on it.

**Related:** [`kubernetes-custom/README.md`](../../instance-template/kubernetes-custom/README.md)
in your instance (the escape hatch's full contract),
[`harbor-accounts.md`](harbor-accounts.md), [`argocd-ops.md`](argocd-ops.md),
[ADR 0007](../adr/0007-app-delivery-boundary.md) (why LLZ ships the platform and
not your delivery chart).

> **What LLZ gives you, and what it doesn't.** LLZ provisions the platform — Argo CD,
> a private Harbor, OpenBao + External Secrets, cert-manager, the gateway, storage,
> the default-deny network baseline — and an `owned` directory to put your manifests
> in. It does **not** ship an application delivery chart, a build pipeline, or an
> opinion about how you produce images ([ADR 0007](../adr/0007-app-delivery-boundary.md)).
> This playbook walks the seam between the two, because every step below is a place
> the platform's contract is easy to guess wrong.

Work through it in order. Each step is independently verifiable, so when something
breaks you know which contract you got wrong rather than debugging four at once.

---

## Step 1 — Prove the escape hatch with a public image

Do this first even though it looks trivial. It exercises the whole GitOps path —
ApplicationSet → Application → namespace creation → sync — with **zero** dependency
on Harbor, OpenBao, or the gateway. If it fails, nothing later can work, and you
have exactly one thing to look at.

```yaml
# kubernetes-custom/namespaces/my-app/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hello
  namespace: my-app          # created for you — do NOT name it apl-* (reserved)
spec:
  replicas: 1
  selector: { matchLabels: { app: hello } }
  template:
    metadata: { labels: { app: hello } }
    spec:
      containers:
        - name: hello
          image: nginxinc/nginx-unprivileged:stable   # public; no pull secret needed
          ports: [{ containerPort: 8080 }]
```

Commit to your default branch. The `instance-custom` ApplicationSet generates one
Application per `namespaces/<ns>/` directory at **sync-wave 10**, and tracks the
branch rather than your release pin — so merging applies it.

**Verify:** `kubectl -n my-app get pods` shows `Running`, and
`kubectl -n argocd get applications | grep my-app` shows `Synced`/`Healthy`.

> **No `kustomization.yaml` in this tree.** The generated Applications use directory
> recursion, and Argo cannot do both — a `kustomization.yaml` would be applied as a
> literal object rather than built. `llz render`/`llz doctor` reject one. Want
> kustomize or Helm? Drop your own Argo `Application` under `namespaces/argocd/`.

---

## Step 2 — Pull from the platform Harbor

Now swap the public image for your own. Two facts decide everything here, and both
are counter-intuitive:

**Your image goes in the `platform` project.** apl-core creates Harbor projects per
APL team plus `platform` and `library` — it does **not** create one named after your
repo, your instance, or your namespace. The push robot (`secret/harbor/robot`) is
scoped to `platform` and nowhere else. Pushing to a project that doesn't exist fails
with a **401, not a 404** — it reads exactly like a bad credential and isn't one.

**Nothing hands your namespace a pull secret.** The `platform` project is private,
and the platform renders a dockerconfigjson only in the cert-automation namespace.
A pod pulling without one fails `no basic auth credentials`.

Give your namespace its own, rendered from the pull-only robot:

```yaml
# kubernetes-custom/namespaces/my-app/harbor-pull.yaml
apiVersion: external-secrets.io/v1        # v1, NOT v1beta1 (apl-core v6 dropped it)
kind: ExternalSecret
metadata:
  name: harbor-pull
  namespace: my-app
spec:
  secretStoreRef: { name: openbao, kind: ClusterSecretStore }
  target:
    name: harbor-pull
    template:
      type: kubernetes.io/dockerconfigjson
      data:
        .dockerconfigjson: |
          {"auths":{ {{- .registry_host | toJson }}:{"username":{{ .username | toJson }},"password":{{ .password | toJson }},"auth":{{ printf "%s:%s" .username .password | b64enc | toJson }}}}}
  data:
    - { secretKey: registry_host, remoteRef: { key: harbor/pull-robot, property: registry_host } }
    - { secretKey: username,      remoteRef: { key: harbor/pull-robot, property: username } }
    - { secretKey: password,      remoteRef: { key: harbor/pull-robot, property: password } }
```

`harbor/*` is on the store's **platform allowlist**, so this path is readable from
any namespace without declaring a team — unlike your *own* app secrets (Step 3).

Then reference it, and use the registry host the robot itself carries:

```bash
# The authoritative registry host — on managed, the domain is Linode's, so never
# hand-assemble it from a domainSuffix.
llz openbao get active secret/harbor/pull-robot registry_host
```

```yaml
    spec:
      imagePullSecrets:
        - name: harbor-pull
      containers:
        - name: hello
          image: <registry_host>/platform/hello:v1
```

**Verify:** the pod pulls. A `401` here is one of the two facts above, not a broken
credential.

---

## Step 3 — Give it a secret

App secrets are **not** on the platform allowlist. The `openbao` store's read
identity covers LLZ's own paths plus the subtree of every team declared in
`spec.teams` — so `myapp/db-password` is a **403 at sync**, the Secret is never
created, and your pod sits in `CreateContainerConfigError` with nothing pointing
back at OpenBao.

Name the path under a declared team's subtree, seed it with the team credential, and
you're covered. The worked example, the seeding commands, and the retrofit caveat
(declaring a team later does **not** retroactively grant the reader policy) are in
**`kubernetes-custom/README.md` → "Secrets: put them under a team subtree"** in your
instance. Don't reinvent it here.

> **After seeding a path late, force one reconcile.** A never-synced ExternalSecret
> backs off up to ~16 minutes, and the store-recovery lane won't help — it fires on
> the *store* going Ready, and your store was healthy all along. See
> [`argocd-ops.md`](argocd-ops.md).

---

## Step 4 — Expose it

Platform services (`console`, `keycloak`, `harbor`, `grafana`, …) are reachable
because each has an **`HTTPRoute`** on apl-core's gateway. On Managed App Platform
the domain is `lke<clusterID>.akamai-apl.net`, provisioned by Linode with a wildcard
certificate — so you do not create DNS records or certificates for a subdomain of it.

Your workload joins the same way. Rather than transcribing a `parentRef` that varies
by platform version, **copy it from a route that already works**:

```bash
kubectl get httproute -A                                   # find a platform route
kubectl get httproute -n <ns> <name> -o yaml | yq .spec.parentRefs
```

then write your own `HTTPRoute` in `namespaces/my-app/` with that `parentRefs` block,
your hostname under the platform domain, and a `backendRefs` pointing at your Service.

**Verify:** `curl -I https://<your-host>` returns your app, with a valid certificate
you did not have to request.

---

## When it doesn't work

The failure signatures worth recognizing, because each reads like something it isn't:

| Symptom | Actual cause |
|---|---|
| `401` on **push** | The Harbor project doesn't exist (it's `platform`), or the robot isn't scoped to it. Not a credential problem. |
| `401` / `no basic auth credentials` on **pull** | No `imagePullSecret` in your namespace — Step 2. |
| Secret never created; pod `CreateContainerConfigError` | ExternalSecret path is outside the platform allowlist *and* outside every `spec.teams` subtree → 403 at sync — Step 3. |
| ExternalSecret still failing minutes after you seeded the path | ESO's never-synced backoff (~16 min). Force one reconcile — [`argocd-ops.md`](argocd-ops.md). |
| Application `OutOfSync` and merging the fix changes nothing | A resource whose health check throws aborts the app's *comparison*, so `selfHeal` never runs — [`argocd-ops.md`](argocd-ops.md). |
| `no matches for kind ExternalSecret in version …/v1beta1` | apl-core v6's ESO serves only `v1`. `llz lint` catches this before it ships. |
| Build pod unschedulable after clearing a `nodeSelector` with `{}` | Helm merges maps key-by-key; `{}` re-inherits the default. Gate the map on a scalar instead. |

---

## What you still own

LLZ deliberately stops here. **Building** the image, **choosing** a CI system, and
**promoting** across environments are yours — [ADR 0007](../adr/0007-app-delivery-boundary.md)
records why, and what the platform guarantees in exchange. If you build in-cluster,
`argoWorkflows: { enabled: true }` in your env's `components` gets you the Workflow
CRDs on managed; note that ephemeral build workspaces belong in an `emptyDir`, not a
PVC (see [`lessons-learned.md`](../lessons-learned.md) — the Linode CSI device-path
flake makes block storage an unreliable scratch disk).

## See also

- [`kubernetes-custom/README.md`](../../instance-template/kubernetes-custom/README.md) — the escape hatch's full contract
- [`harbor-accounts.md`](harbor-accounts.md) — projects, robots, and human logins
- [`argocd-ops.md`](argocd-ops.md) — sync/health triage and recovery
- [`operator-onboarding.md`](operator-onboarding.md) — getting your own access first
