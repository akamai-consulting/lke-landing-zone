package main

// ci_bootstrap_cluster_manifests.go holds the manifest builders for `llz ci
// bootstrap-cluster` — the Go map literals server-side-applied in place of the
// module's `kubectl_manifest` resources (yamlencode(...) + server_side_apply).
// Each function is a faithful port of the corresponding resource in
// terraform-modules/llz-cluster-bootstrap/main.tf; the labels, annotations,
// sourceRepos pin, and full syncPolicy (retry 40 @ 90s cap,
// SkipDryRunOnMissingResource) are carried verbatim — see the module for the
// per-field rationale.

import (
	"encoding/base64"
	"encoding/json"
)

// base64Auth is the `username:password` docker-auth blob (mirrors the module's
// base64encode("${username}:${token}")).
func base64Auth(username, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
}

// managedByBootstrapLabel marks every namespace this bootstrap owns (matches the
// module's `lke-landing-zone.akamai.io/managed-by-bootstrap: "true"`).
const managedByBootstrapLabel = "lke-landing-zone.akamai.io/managed-by-bootstrap"

// aplOperatorNamespaceManifest — the apl-operator namespace pre-tagged with the
// three Helm ownership markers so the chart's 00-namespace.yaml adopts it
// instead of colliding on install.
func aplOperatorNamespaceManifest() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": "apl-operator",
			"labels": map[string]any{
				managedByBootstrapLabel:        "true",
				"app.kubernetes.io/managed-by": "Helm",
			},
			"annotations": map[string]any{
				"meta.helm.sh/release-name":      "apl",
				"meta.helm.sh/release-namespace": "apl-operator",
			},
		},
	}
}

// argocdNamespaceManifest — the argocd namespace, stamped with the apl-core
// `name=<ns>` convention label its NetworkPolicies select on.
func argocdNamespaceManifest() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": "argocd",
			"labels": map[string]any{
				"name":                  "argocd",
				managedByBootstrapLabel: "true",
			},
		},
	}
}

// ghcrOCIRepoSecretManifest — the ArgoCD repository Secret that authenticates to
// GHCR for the first-party OCI Helm charts (only created when a token is set;
// the charts are public otherwise).
func ghcrOCIRepoSecretManifest(o bootstrapClusterOpts) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      "ghcr-charts-oci",
			"namespace": "argocd",
			"labels": map[string]any{
				"argocd.argoproj.io/secret-type": "repository",
			},
		},
		"type": "Opaque",
		"stringData": map[string]any{
			"type":      "helm",
			"url":       "ghcr.io/" + o.upstreamOrg + "/charts",
			"enableOCI": "true",
			"username":  o.ghcrUsername,
			"password":  o.ghcrToken,
		},
	}
}

// ghcrPullSecretManifest — the dockerconfigjson image-pull Secret for private
// ghcr.io images, using the same GHCR read token.
func ghcrPullSecretManifest(o bootstrapClusterOpts) map[string]any {
	dockerconfig, _ := json.Marshal(map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]any{
				"username": o.ghcrUsername,
				"password": o.ghcrToken,
				"auth":     base64Auth(o.ghcrUsername, o.ghcrToken),
			},
		},
	})
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      "ghcr-pull-secret",
			"namespace": "kube-system",
		},
		"type": "kubernetes.io/dockerconfigjson",
		"stringData": map[string]any{
			".dockerconfigjson": string(dockerconfig),
		},
	}
}

// platformBootstrapAppProjectManifest — the source-pinned AppProject for the
// bootstrap Application (sourceRepos pinned to the instance repo + the template
// repo; destinations/whitelists permissive because the manifest tree spans many
// namespaces + kinds).
func platformBootstrapAppProjectManifest(o bootstrapClusterOpts) map[string]any {
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "AppProject",
		"metadata": map[string]any{
			"name":      "platform-bootstrap",
			"namespace": "argocd",
		},
		"spec": map[string]any{
			"description": "Seed project for the apl-values manifest tree app-of-apps. Source-pinned to the instance repo (the platform-bootstrap overlay) + the template repo (the shared llz-secret-store ClusterSecretStore tree at platform-apl/), both over HTTPS.",
			"sourceRepos": []any{
				"https://github.com/" + o.instanceRepo + ".git",
				"https://github.com/" + o.upstreamOrg + "/lke-landing-zone.git",
			},
			"destinations": []any{
				map[string]any{
					"server":    "https://kubernetes.default.svc",
					"namespace": "*",
				},
			},
			"clusterResourceWhitelist": []any{
				map[string]any{"group": "*", "kind": "*"},
			},
			"namespaceResourceWhitelist": []any{
				map[string]any{"group": "*", "kind": "*"},
			},
		},
	}
}

// platformBootstrapApplicationManifest — points ArgoCD at the instance repo's
// apl-values/<env>/manifest tree; automated sync with prune + selfHeal and the
// load-bearing retry budget (40 @ 90s cap) + SkipDryRunOnMissingResource.
func platformBootstrapApplicationManifest(o bootstrapClusterOpts) map[string]any {
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      "platform-bootstrap",
			"namespace": "argocd",
		},
		"spec": map[string]any{
			"project": "platform-bootstrap",
			"source": map[string]any{
				"repoURL":        "https://github.com/" + o.instanceRepo + ".git",
				"targetRevision": o.appsRepoRevision,
				"path":           "apl-values/" + o.env + "/manifest",
			},
			"destination": map[string]any{
				"server":    "https://kubernetes.default.svc",
				"namespace": "argocd",
			},
			"syncPolicy": map[string]any{
				"automated": map[string]any{
					"prune":    true,
					"selfHeal": true,
				},
				"retry":       argoRetry(),
				"syncOptions": []any{"CreateNamespace=true", "ServerSideApply=true", "SkipDryRunOnMissingResource=true"},
			},
		},
	}
}

// secretStoreApplicationManifest — the carved llz-secret-store Application
// (blast-radius isolation for the openbao ClusterSecretStore), sourced from the
// env-agnostic template-repo tree at the pinned template ref; prune off.
func secretStoreApplicationManifest(o bootstrapClusterOpts) map[string]any {
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      "llz-secret-store",
			"namespace": "argocd",
		},
		"spec": map[string]any{
			"project": "platform-bootstrap",
			"source": map[string]any{
				"repoURL":        "https://github.com/" + o.upstreamOrg + "/lke-landing-zone.git",
				"targetRevision": o.templateRef,
				"path":           "platform-apl/manifest-secret-store",
			},
			"destination": map[string]any{
				"server":    "https://kubernetes.default.svc",
				"namespace": "argocd",
			},
			"syncPolicy": map[string]any{
				"automated": map[string]any{
					"prune":    false,
					"selfHeal": true,
				},
				"retry":       argoRetry(),
				"syncOptions": []any{"ServerSideApply=true", "SkipDryRunOnMissingResource=true"},
			},
		},
	}
}

// argoRetry is the shared first-boot retry budget (~1h): 40 retries, 15s→90s
// exponential backoff. Load-bearing on first boot while the async OpenBao →
// ESO → ClusterSecretStore chain converges (see the module's syncPolicy note).
func argoRetry() map[string]any {
	return map[string]any{
		"limit": 40,
		"backoff": map[string]any{
			"duration":    "15s",
			"factor":      2,
			"maxDuration": "90s",
		},
	}
}

// bootstrapNextSteps is the post-apply operator checklist (ported from the
// workspace's outputs.tf next_steps), with the deployment name filled in.
func bootstrapNextSteps(env string) string {
	return `
── Post-apply checklist ────────────────────────────────────────────────

Apl-core is installed. The apl-operator drives the helmfile pipeline
which installs ~40 components over 10–15 minutes; downstream readiness
is observed via Argo CD, not this command.

1. Watch the apl-operator log:

     kubectl -n apl-operator logs -l app.kubernetes.io/name=apl-operator -f

2. Confirm Argo CD has reconciled the in-repo manifest/ tree:

     kubectl -n argocd get applications

3. Once OpenBao pods are Running, bootstrap OpenBao:

     .github/workflows/bootstrap-openbao.yml → workflow_dispatch
     region: ` + env + `

────────────────────────────────────────────────────────────────────────
`
}
