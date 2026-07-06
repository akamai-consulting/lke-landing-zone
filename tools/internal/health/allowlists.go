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
		{"gitops-global", "apl-core's global-values Argo app is hardwired to clone the in-cluster gitea (gitea-http.gitea.svc), which this landing zone obsoletes — otomi.git points at the external GitHub repo. Bound deep in apl-core, not our config; deferred until apl-core sources gitops-global from otomi.git"},
		{"team-[a-z0-9-]+-values-gitops", "apl-core/otomi generates a per-team values-gitops Application pointing at env/teams/<team>/sealedsecrets — a path that does not exist in this landing zone (we use ESO + OpenBao, not otomi per-team sealed-secrets), so it sits Unknown with a ComparisonError ('app path does not exist'). Same class as gitops-global: an apl-core-internal app this LZ obsoletes, not our config; deferred so it can't pin the convergence gate."},
		{"gitops-ns-apl-[a-z0-9-]+", "apl-core's v6 operator creates one gitops Application per env/manifests/namespaces/<ns> dir (apply-as-apps.ts addGitOpsApps); the apl-*-prefixed namespaces (apl-secrets, apl-users) are operator-owned SealedSecret stores. This landing zone never populates that path (it uses ESO + OpenBao, not otomi SealedSecrets), so the apps sit Unknown with a ComparisonError ('env/manifests/namespaces/apl-... app path does not exist'). Same class as gitops-global / team-*-values-gitops — an apl-core-internal app this LZ obsoletes, deferred so it can't pin the convergence gate."},
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
		{"llz-cert-automation/harbor-docker-config", "reads the Harbor robot creds at secret/harbor/robot, which are seeded by the in-cluster harbor reconciler (llz-reconciler --reconcile-harbor, formerly the harbor-robot-provisioner CronJob) only AFTER Harbor's registry is up — so on a fresh bootstrap it sits Ready=False (SecretSyncedError: could not get secret data from provider) for the first few minutes until the reconciler's first tick writes the path and the ExternalSecret re-syncs (refreshInterval 1m). It feeds ONLY the cert-automation HAProxy-rebuild workflow, which runs on cert rotation (~80 days out), so it is off the bootstrap critical path and must not pin the convergence gate. Deferred; it converges shortly after Harbor without operator action."},
		{"llz-reconciler/harbor-admin-password", "reads secret/harbor/admin, which apl-core's Harbor Helm chart only generates AFTER Harbor deploys (the harbor-admin-push PushSecret then mirrors it into OpenBao) — so on a fresh bootstrap it sits Ready=False (SecretSyncedError) until Harbor is up. The harbor reconciler mounts the resulting Secret OPTIONAL and no-ops until it appears, and the ExternalSecret sits at a high sync-wave so it gates nothing; it is entirely off the bootstrap critical path. Same class as harbor-docker-config — deferred; it converges shortly after Harbor without operator action."},
	}
}

// NPExternalDepNamespaces are namespaces whose default-deny NetworkPolicies
// arrive only once an operator-deferred Application syncs.
func NPExternalDepNamespaces() []DepEntry {
	return []DepEntry{}
}
