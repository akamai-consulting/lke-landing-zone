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
		{"external-dns-external-dns", "LINODE_DNS_TOKEN not seeded — run bootstrap-dns.yml + re-apply TF so apl-values dns.provider.linode.apiToken is populated"},
		{"istio-system-oauth2-proxy", "Keycloak OIDC issuer (keycloak.<domain>) not resolvable until DNS is wired — run bootstrap-dns.yml; deferred alongside external-dns"},
		{"gitops-global", "apl-core's global-values Argo app is hardwired to clone the in-cluster gitea (gitea-http.gitea.svc), which this landing zone obsoletes — otomi.git points at the external GitHub repo. Bound deep in apl-core, not our config; deferred until apl-core sources gitops-global from otomi.git"},
		{"team-[a-z0-9-]+-values-gitops", "apl-core/otomi generates a per-team values-gitops Application pointing at env/teams/<team>/sealedsecrets — a path that does not exist in this landing zone (we use ESO + OpenBao, not otomi per-team sealed-secrets), so it sits Unknown with a ComparisonError ('app path does not exist'). Same class as gitops-global: an apl-core-internal app this LZ obsoletes, not our config; deferred so it can't pin the convergence gate."},
	}
}

// ExternalDepWorkloads are Deployments/StatefulSets/Pods deferred on the same
// operator-supplied inputs (namespace/name patterns).
func ExternalDepWorkloads() []DepEntry {
	return []DepEntry{
		{"external-dns/external-dns", "LINODE_DNS_TOKEN not seeded — run bootstrap-dns.yml + re-apply TF"},
		{"kube-system/linode-internal-cidr-firewall", "release.yml build-firewall-controller has not run — ImagePullBackOff until the image is pushed to ghcr.io and the App's image.tag is pinned"},
		{"istio-system/oauth2-proxy", "init-blocks on the Keycloak OIDC issuer URL — unresolvable until DNS is wired (run bootstrap-dns.yml)"},
	}
}

// ExternalDepExternalSecrets are ExternalSecrets expected Ready=False until an
// operator-supplied input arrives.
func ExternalDepExternalSecrets() []DepEntry {
	return []DepEntry{}
}

// SecretPlaneSettlingSecrets are the workload-critical ExternalSecrets ESO syncs
// from OpenBao AFTER the ClusterSecretStore goes Ready. Their dependent
// workloads — harbor-registry (registry-s3), loki-0 (object-store), and the
// harbor image-pull path (harbor-docker-config) — plus the Services,
// Deployments, and app-of-apps that fan out from them, sit transiently
// unhealthy (CreateContainerConfigError / 0-endpoints / OutOfSync-Missing)
// during that sync window. The converge fail-fast grace stays open until each of
// these is Ready, so the gate polls the window against its budget instead of
// aborting the instant the store is Ready. Values are namespace/name.
func SecretPlaneSettlingSecrets() []string {
	return []string{
		"harbor/harbor-registry-s3",
		"monitoring/loki-object-store",
		"llz-cert-automation/harbor-docker-config",
	}
}

// NPExternalDepNamespaces are namespaces whose default-deny NetworkPolicies
// arrive only once an operator-deferred Application syncs.
func NPExternalDepNamespaces() []DepEntry {
	return []DepEntry{}
}
