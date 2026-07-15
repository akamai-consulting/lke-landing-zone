package health

// allowlists.go is the data the classifiers consult to route known-deferred /
// Phase-1-cascade resources to the deferred/pending channels instead of failing
// the converge. Mirrors the EXTERNAL_DEP_* / PHASE_1_* arrays in
// check-cluster-health.sh; keep the two in sync.

// Phase1PendingApps are the ArgoCD Application names whose OutOfSync/Missing/
// Progressing state is expected until OpenBao is bootstrapped (CA chain cascade).
func Phase1PendingApps() []string {
	return []string{"platform-openbao"}
}

// Phase1PendingWorkloads are the namespace/name prefixes whose unhealthiness is
// expected until OpenBao is bootstrapped.
func Phase1PendingWorkloads() []string {
	return []string{"openbao/platform-openbao"}
}

// ExternalDepApps are ArgoCD Applications blocked on an operator-supplied input
// (a built image, the Linode DNS token) — deferred, not failed.
func ExternalDepApps() []DepEntry {
	return []DepEntry{
		{"linode-internal-cidr-firewall", "release.yml build job has not run yet — firewall-controller image not at ghcr.io; publish a release tag (or fire harbor-ready) to build+push and pin the App's image.tag"},
		{"external-dns-external-dns", "LINODE_DNS_TOKEN not provisioned — re-apply TF so apl-values dns.provider.linode.apiToken is populated (from TF_VAR_linode_dns_token)"},
		{"istio-system-oauth2-proxy", "Keycloak OIDC issuer (keycloak.<domain>) not resolvable until DNS is wired; deferred alongside external-dns"},
		{"gitops-global", "apl-core's global-values Argo app. Its source.repoURL is otomi.git.repoUrl — the external GitHub repo, NOT the in-cluster gitea (v6 source: apply-as-apps.ts addGitOpsApps → getStoredGitRepoConfig → getArgocdGitopsManifest; gitea-http.gitea.svc is only apl-core's GIT_LEGACY_CONFIG default, overridden by BYO-git, and every gitea code path is gated on repoUrl containing gitea-http). Verified Synced/Healthy on v6 e2e converges (2026-07-15, runs 29374149739/29380924847/29409532035). This deferral is now a conservative no-op guarding only the brief early-bootstrap window before the operator first pushes env/manifests and the app first syncs — safe to drop once confirmed Synced/Healthy at gate time across more runs. (The earlier 'hardwired to clone gitea / fails no such host' rationale was incorrect.)"},
		{"team-[a-z0-9-]+-values-gitops", "apl-core/otomi generates a per-team values-gitops Application pointing at env/teams/<team>/sealedsecrets in the operator-managed values branch. This is an apl-core-internal app the LZ does not drive (we use ESO + OpenBao, not otomi per-team sealed-secrets). On v6 e2e it reports Synced/Healthy (the operator does push the env/teams tree), so this deferral is a conservative no-op keeping an app the LZ doesn't own off the convergence gate — same class as gitops-global. (Earlier 'path does not exist / Unknown ComparisonError' rationale was not borne out by the live e2e; re-confirm across runs before dropping.)"},
		{"gitops-ns-apl-[a-z0-9-]+", "apl-core's v6 operator creates one gitops Application per env/manifests/namespaces/<ns> dir (apply-as-apps.ts addGitOpsApps); the apl-*-prefixed namespaces (apl-secrets, apl-users) are operator-owned SealedSecret stores. On v6 e2e these report Synced/Healthy — the operator DOES push env/manifests/namespaces/apl-secrets/sealedsecrets/* to its values branch (verified in the live env/ tree), so this deferral is a conservative no-op keeping apl-core-internal apps the LZ doesn't drive off the convergence gate — same class as gitops-global. (Earlier 'path does not exist / Unknown ComparisonError' rationale was not borne out by the live e2e; re-confirm across runs before dropping.)"},
	}
}

// ExternalDepWorkloads are Deployments/StatefulSets/Pods deferred on the same
// operator-supplied inputs (namespace/name patterns).
func ExternalDepWorkloads() []DepEntry {
	return []DepEntry{
		{"external-dns/external-dns", "LINODE_DNS_TOKEN not provisioned — re-apply TF (from TF_VAR_linode_dns_token)"},
		{"kube-system/linode-internal-cidr-firewall", "release.yml build-firewall-controller has not run — ImagePullBackOff until the image is pushed to ghcr.io and the App's image.tag is pinned"},
		{"istio-system/oauth2-proxy", "init-blocks on the Keycloak OIDC issuer URL — unresolvable until DNS is wired"},
		{"otomi/otomi-api", "apl-core-internal otomi API server — this landing zone drives gitops via external GitHub + ESO/OpenBao, not the otomi console/API, so otomi-api is not configured or load-bearing here (zero references in apl-values; same apl-core-internal class as gitops-global). It CrashLoopBackOffs on a fresh install; the exact cause is captured by the on-failure workload-log diagnostic (llz-bootstrap-openbao.yml). Deferred at pod level so an apl-core-internal component the LZ doesn't drive can't pin the convergence gate — revisit (un-defer + fix) if those logs show a shared-dependency failure."},
		{"llz-reconciler/llz-reconciler", "the reconciler's main container reads LINODE_TOKEN from the ESO-synced linode-api-token Secret (secretKeyRef), so on a fresh bootstrap the pod sits in CreateContainerConfigError for the first ~1-2 min after the openbao ClusterSecretStore goes Ready — until ESO first-syncs secret/linode/api-token → the linode-api-token Secret and the container can be created with the token populated. It self-heals with no operator action. The reconciler is a day-2 observability/reconcile signal whose own health lives on its metrics surface (llz_reconcile_up / llz_reconcile_errors_total), NOT the bootstrap critical path, so it must not pin the convergence gate — a genuinely broken reconciler is caught by its metrics/alerts and the e2e functional job, not by hard-failing converge. Deferred; it converges shortly after the openbao store without operator action."},
	}
}

// ExternalDepExternalSecrets are ExternalSecrets expected Ready=False until an
// operator-supplied input arrives.
func ExternalDepExternalSecrets() []DepEntry {
	return []DepEntry{
		{"llz-cert-automation/harbor-docker-config", "reads the Harbor robot creds at secret/harbor/robot, which are seeded by the harbor-robot-provisioner CronJob (schedule */5) only AFTER Harbor's registry is up — so on a fresh bootstrap it sits Ready=False (SecretSyncedError: could not get secret data from provider) for the first few minutes until the CronJob's first tick writes the path and the ExternalSecret re-syncs (refreshInterval 1m). It feeds ONLY the cert-automation HAProxy-rebuild workflow, which runs on cert rotation (~80 days out), so it is off the bootstrap critical path and must not pin the convergence gate. Deferred; it converges shortly after Harbor without operator action."},
	}
}

// NPExternalDepNamespaces are namespaces whose default-deny NetworkPolicies
// arrive only once an operator-deferred Application syncs.
func NPExternalDepNamespaces() []DepEntry {
	return []DepEntry{}
}
