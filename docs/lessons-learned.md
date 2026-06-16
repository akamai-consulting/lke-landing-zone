# Lessons Learned

Hard-won, non-obvious gotchas for agents and contributors working in this repo —
the kind of thing you can't infer from the code alone and that has bitten work
before. Read this alongside [AGENTS.md](../AGENTS.md). When a lesson here turns out
to be stale, fix it in place rather than working around it.

## Repo topology & where to base work

- **Base work on the canonical upstream's default branch, not a personal fork.**
  Terraform module `git::` sources, branch pushes, and PRs must target the
  canonical repo — a fork's default branch is often many commits stale even when
  the content is already merged upstream under different SHAs. Make sure `gh` is
  wired to the canonical repo and verify with `git fetch <upstream>` +
  `git log <upstream>/<default-branch>` before basing new work.
- **Workflow is stacked PRs.** Branch off the canonical default branch (or the
  parent feature branch in a stack), not a stale local branch.

## Reusable-artifact structure

- Reusable, separately-published artifacts live in **two mirrored top-level
  trees**: `kubernetes-charts/` (first-party Helm charts, published to OCI,
  versioned independently per `Chart.yaml`) and `terraform-modules/` (shared TF
  modules, published as `git::` sources pinned to the one umbrella release tag —
  not per-module; see RELEASING.md). In-repo *consumers* (`terraform-iac-bootstrap/`
  roots, `apl-values/`) stay in their own trees — shared-vs-custom is structural.
- **The OCI registry path stays `charts`** (`CHARTS_REPO_PATH` in
  `publish-charts.yml`) even though the source dir was renamed from `charts/` to
  `kubernetes-charts/`. Argo `repoURL`/`chart` coordinates are stable — don't
  "fix" them to match the dir name.
- **CI gates key on path.** Terraform gates include both `terraform-iac-bootstrap/**`
  and `terraform-modules/**`; Helm gates key on `kubernetes-charts/`. Moving a file
  between trees can silently change which gates run.
- **TF module sibling refs stay relative** (`../lke-node-firewall`) — they resolve
  inside the `git::` checkout. Never rewrite an internal sibling reference to a
  `git::` source; that would unpin the two halves from the same tag.

## Templatization / helmification scope

- **No service-/org-prefix on reusable references.** Chart resource names, release
  names, and generated Argo Application names stay generic so two system teams
  don't collide. The only exceptions are existing load-bearing identities, kept as
  overridable value defaults (e.g. a live `<service>-openbao` StatefulSet release
  name pinned via values).
- **The product is out of scope — only the platform is reusable.** A sibling team's
  application workloads are the product, so they are *not* helmified here; the
  reusable unit is the platform that runs them.

## LKE-Enterprise constraints

- **Both clusters run LKE-Enterprise** (`k8s_version` carries a `+lke` suffix),
  not standard LKE. The Terraform modules look generic, so this is **not derivable
  from code** — but it materially changes correct behavior.
- **Secret rotation is `lke-admin-token` only**, via the Linode delete-kubeconfig
  API (`DELETE /v4/lke/clusters/{id}/kubeconfig`). There is **no** sanctioned batch
  SA-token rotation and **no** `regenerate` service-token call on LKE-E. **Never
  `kubectl delete` the `lke-admin-token` Secret** — on LKE-E it is not regenerated.
- Rotation deliberately reuses the broad shared `LINODE_API_TOKEN` rather than a
  Kubernetes-scoped PAT — an **accepted, documented deviation**
  (`docs/runbooks/lke-admin-rotation.md`). Don't re-propose a least-privilege PAT or
  a scope preflight for it.

## CI / GitHub Actions

- **The repo is fully on github.com + GitHub-hosted `ubuntu-latest`** — the old
  Linode GitHub Enterprise / self-hosted-runner model (manual `curl` installs to
  `$HOME/.local/bin`) is **gone**, including `template-scripts/ci/setup-runner.sh`. Both
  `.github/workflows/` and `instance-template/.github/workflows/` follow this.
- **Install CLI tools with SHA-pinned marketplace setup actions** (version pinned
  from the `env:` block), not `curl | tar`. The real Docker login action is
  `docker/login-action` — `actions/docker-login-action` does **not** exist (it was
  a GHE-mirror typo). Tools baked into the `ci-terraform`/`ci-kubernetes` images
  don't need re-install in `container:` jobs.
- **GHCR auth** for publish/CI workflows uses the built-in `GITHUB_TOKEN` +
  `permissions: packages: write|read`, namespace from `${GITHUB_REPOSITORY_OWNER,,}`.
- **Operational-reachability caveat:** the migrated operational workflows
  (openbao-auto-unseal, cluster-health, secret-rotation, terraform apply,
  e2e, etc.) now run from GitHub's cloud, **not** inside the cluster network. This
  repo manages a Linode Cloud Firewall that may allowlist only specific CIDRs — if
  the LKE API / OpenBao / Linode API endpoints are CIDR-restricted, hosted runners
  can be blocked at runtime. Verify allowlisting/connectivity before relying on
  these in prod.

## Placeholders to revisit

- **OTel Collector TLS hostnames are internal placeholders** —
  `otel.<env>.internal` and `otel.<secondary-env>.internal` (in the production and
  production-secondary overlay kustomizations). External DNS for these doesn't
  exist yet; when it's created, update the OTel collector `tls.host` value in both
  overlay patches to the real public FQDNs.

## Operational scars (observed failure modes)

These are failure modes hit while operating this Linode LKE-Enterprise +
apl-core stack. They are *why* many non-obvious chart/module defaults exist — the
AGENTS.md "scars as defaults" rule. Treat each as a failure mode to preserve a
default against, not something to "clean up." Version-specific notes (apl-core
5.0.0, etc.) may drift — verify against the live chart version before acting.

### NetworkPolicy & default-deny (Cilium on LKE-E)

- **apiserver-egress NPs must allow port 6443, not just 443.** Cilium evaluates
  policy *post-DNAT*, so egress to the kube-apiserver must allow `6443` alongside
  `443` or all API calls are silently dropped.
- **NP sync-wave race on cold bootstrap.** Helm-templated NPs land in the same Argo
  wave as the pods they protect; cilium-agent BPF programming is async, so
  workloads with short retry-then-fatal loops (e.g. Harbor, 60s) crashloop before
  NPs enforce. Annotate NPs with `sync-wave: "-10"`.
- **ESO `external-secrets-default-deny` has no matching allow-ingress**, so the
  validating webhook times out on every `ClusterSecretStore`/`ExternalSecret`
  mutation. Needs an explicit allow.
- **Istio sidecar needs istiod egress** (`tcp/15012` to `istio-system/app=istiod`)
  in default-deny namespaces. Without it the sidecar's iptables redirect installs
  but Envoy aborts (no cert) and **all** pod egress (including kube-apiserver) dies
  — surfaces as cryptic "connection refused" crashloops; the real fix is two lines
  of NetworkPolicy.
- **`harbor-default-deny` blocks the CNPG operator.** It lacks a `cnpg-system`
  allow, so the operator can't poll Postgres status on `:8000`, the Harbor DB
  replica never starts, and convergence stalls. (Only Harbor has a default-deny;
  gitea/keycloak don't.)
- **argo-events v1.9+ relabeled pods.** EventBus/Sensor/EventSource pods now carry
  `controller=*-controller` labels instead of `app.kubernetes.io/component=*`. NPs
  using the old selectors match nothing → JetStream "Waiting for routing" loop.
- **apl-core 5.0.0 Gateway API label change.** Gateway pods are labeled
  `gateway.networking.k8s.io/gateway-name=<name>`, **not** legacy
  `app=gateway-<name>`. NPs on the legacy label silently match nothing → intra-
  cluster `*.primary.internal` HTTPS hangs while the external LB still works.

### Linode / LKE Terraform lifecycle

- **LKE pool inline-drift destroys the pool.** The Linode TF provider echoes pools
  back onto `linode_lke_cluster`; without `ignore_changes = [pool]` a refresh plans
  to null the pool and Linode interprets that as "delete the pool." Keep the
  `ignore_changes`.
- **Silent cluster-create hang = the node pool never provisioned.** A fresh LKE-E
  apply stuck on `linode_lke_cluster.this: Still creating...` to the job timeout
  means Linode never brought up the worker nodes, so the cluster never reaches
  Ready and no kubeconfig is ever produced (→ "no kubeconfig in the UI"). This is
  NOT reliably an orphan/quota problem: it has been hit with preflight reporting
  `Orphaned total: 0` and several *legitimate* clusters on a shared account — the
  pool still never came up. There is no Linode compute-quota
  API, so any "quota exceeded" claim is unverifiable from automation; orphan-
  sweeping is a guess, not a diagnosis. The cluster resource is pool-less (the
  node pool is the separate `linode_lke_node_pool`), so the hang is the cluster's
  own create, before the pool resource even applies. Authoritative signal:
  `GET /v4beta/lke/clusters/<id>/pools` (node status) + the instance list — did
  the `lke<id>-*` workers appear at all? Never allocate → either capacity (region
  stock-out of the `node_type`, or an account instance cap) OR the cluster can't
  get **network infra it depends on** — most importantly a **VPC** (LKE-E requires
  one; at the account's VPC quota the create hangs with no API error). Appear but
  never join → node firewall/VPC misconfig. Disambiguate by retrying a different
  region/type and by checking VPC/firewall/NodeBalancer counts against quota;
  escalate to Linode support for a confirmed compute limit. Don't assume orphans.
  **Confirmed root cause:** VPC-quota exhaustion from a
  per-cycle leak — `linode_lke_cluster` was given `subnet_id` but **not** `vpc_id`,
  so LKE-E ignores the BYO VPC and provisions its own `lke<id>` one, orphaning the
  module's `<cluster_label>-vpc` on every run (and the node firewall leaked too,
  because teardown looked it up by the wrong label). Fixed by binding `vpc_id` and
  by reaping the firewall/VPC on destroy (after the NodeBalancer sweep) and dropping
  zombie-cluster state so the bootstrap destroy doesn't hang. Node type / vCPU quota
  were red herrings.
- **Cluster destroy needs `Events:Read` on the token.** Destroy issues the delete
  then 401s in the post-delete waiter (`/account/events`) unless `LINODE_API_TOKEN`
  carries Events:Read scope.
- **The default StorageClass is Flux-managed.** `linode-block-storage-retain` is
  owned by Flux's `workload` HelmRelease, so annotation patches (e.g. demoting it
  from default) revert on Flux's ~10m reconcile. Fall back to per-chart
  `storageClassName:` overrides.

### OpenBao / ESO / cert rotation

- **OpenBao raft-join is a chicken-and-egg.** Followers never auto-join (no
  `retry_join` in the chart), and the bootstrap `openbao-tls` cert lacks pod SANs
  while cert-manager can't re-issue until OpenBao is already up. The bootstrap path
  must account for both.
- **ESO `caProvider` goes stale after cert-manager rotation.** ESO reads the CA
  once at pod start and caches it forever; after `openbao-tls` rotates, ESO holds
  the old CA → "x509: unknown authority" on every reconcile. The
  `eso-cert-watcher` singleton polls the Secret's resourceVersion every 60s and
  restarts ESO only on real changes.
- **`apl_pipeline_ready` must not gate on ESO.** ESO is installed by the bootstrap
  tree (wave -15) that the gate `depends_on`, so waiting for it is circular and
  deadlocks fresh bootstraps (apl-core runs sealed-secrets by default).

### Argo / destroy / cold-start

- **Argo child Apps must inherit the env revision.** Hardcoding
  `targetRevision: main` strands branch-based bootstraps. Drive it from the per-env
  local-config ConfigMap + kustomize replacements in the **overlay** (not the base
  — base evaluates first).
- **Destroy deadlocks on Argo finalizers.** `helm_release.apl` uninstall hangs
  forever because ~60 Argo Applications keep their `resources-finalizer` after the
  controller is gone; stale `Available=False` APIServices also stall namespace GC.
  Manual unwedge is patching finalizers to `[]`.
- **apl-operator cold-start gitea DNS race.** The initial otomi/values push is lost
  when `gitea-http` DNS isn't up yet; the operator loops on
  `waitTillGitRepoAvailable` forever and argocd/kyverno/cert-manager stay empty.
  Fix is a stage-0 self-heal restart in `apl_pipeline_ready`.

### apl-core 5.0.0 integration quirks

- **argo-cd SSH known-hosts schema mismatch.** apl-core's `_rawValues` pipeline
  rewrites `configs.ssh.*` into a legacy map shape while the chart wants a string,
  so apl-operator loops install forever. Workaround: `configs.ssh.create: false` +
  a TF-managed `argocd-ssh-known-hosts-cm`.
- **helmfile phase ordering** is kyverno(01) → gitea(03) → oauth2-proxy(70) with
  `wait:true` between phases, and there's no user-extensible early custom-policy
  hook — so the PVC Kyverno policy stays a TF `null_resource` applied at
  kyverno-ready.
- **oauth2-proxy init has no Otomi CA trust.** `wait-for-keycloak` runs in vanilla
  `curlimages/curl` against the Otomi-signed wildcard cert → exit 60, init loops
  forever. A Kyverno mutation adds `-k`; only surfaces *after* the Gateway NP
  selector is fixed.
- **Loki S3 wiring** (apl-core): Loki runs in `monitoring` (not grafana); needs
  `_rawValues.loki.schemaConfig.object_store: s3` (the chart won't derive it),
  creds at `singleBinary.extraEnvFrom` (no top-level `extraEnvFrom`), and the
  `loki-object-store` secret in `monitoring`. Miss any → zero chunks persisted.

### Build, CI & shell scripting

- **`pipefail` + `cmd | jq || echo default` double-emits.** When `cmd` exits
  non-zero but still prints stdout (`bao status` exits 2 when sealed; `grep -v`
  exits 1 on no match), the `|| echo` fallback fires **and** appends to jq's output
  → corrupt multiline value. Capture raw output first, then parse.
- **`kubectl wait --for=condition` errors immediately on a missing resource.**
  `--timeout` only governs an *existing* resource; on a not-yet-created one kubectl
  exits NotFound right away. Poll for existence first, then wait on the condition.
- **Ask for the kubeconfig.** Versioned downloads change filenames; ask the user to
  paste their `export KUBECONFIG=...` line rather than guessing a path.


