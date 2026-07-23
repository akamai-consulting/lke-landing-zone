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

// llzOpenbaoNamespaceManifest — the llz-openbao namespace, pre-created so the
// bootstrap-openbao seal-key seed lands immediately instead of waiting ~40s for
// the llz-cluster-foundation Argo app (wave -20) to create it (measured: the
// seal-key step spent 43 of its 44s purely on this wait — e2e timing artifacts,
// run 29651276573). Same SSA pre-create + Argo adoption pattern as the argocd /
// apl-operator namespaces above. Stamped restricted-PSS + monitoring to match
// llz-cluster-foundation's namespaces.yaml so Argo's later SSA is a clean adopt
// (the PSS *-version labels land when the chart syncs; enforce=restricted alone
// already restricts at the latest version until then). The seal-key step keeps
// its own namespace-wait as the safety net if this ever doesn't run.
func llzOpenbaoNamespaceManifest() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": "llz-openbao",
			"labels": map[string]any{
				"monitoring":                         "enabled",
				"pod-security.kubernetes.io/enforce": "restricted",
				"pod-security.kubernetes.io/warn":    "restricted",
				"pod-security.kubernetes.io/audit":   "restricted",
				managedByBootstrapLabel:              "true",
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

// instanceRepoArgoSecretManifest — the ArgoCD repository Secret that authenticates
// to the PRIVATE instance repo over HTTPS so the platform-bootstrap Application can
// list refs + pull apl-values/<env>/manifest. Only created when a token is set (a
// public instance repo needs none).
//
// This is load-bearing ONLY on managed apl-core: on self-install apl-core's own
// ArgoCD carried the instance-repo credential via otomi.git.password (the values
// repo IS the instance repo), so platform-bootstrap rode on it. On managed,
// apl-core's otomi.git points at Linode's in-cluster gitea — NOT the instance repo
// — so nothing else provides this credential, and without it platform-bootstrap
// dead-ends on "authentication required: Repository not found", never deploying its
// OpenBao child (the exact convergence deadlock). x-access-token:<PAT> is the same
// basic-auth `llz` already uses for the values repo (see authedGitURL).
func instanceRepoArgoSecretManifest(o bootstrapClusterOpts) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      "instance-repo",
			"namespace": "argocd",
			"labels": map[string]any{
				"argocd.argoproj.io/secret-type": "repository",
			},
		},
		"type": "Opaque",
		"stringData": map[string]any{
			"type":     "git",
			"url":      "https://github.com/" + o.instanceRepo + ".git",
			"username": "x-access-token",
			"password": o.instanceRepoToken,
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
