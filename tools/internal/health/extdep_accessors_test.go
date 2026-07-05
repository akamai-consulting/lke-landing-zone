package health

import "testing"

// The Cert operator-deferred allowlist ships empty; pin that so a future
// non-empty entry is a deliberate edit, and cover the otherwise-unreferenced
// accessor.
func TestExternalDepCertsEmpty(t *testing.T) {
	if got := ExternalDepCerts(); len(got) != 0 {
		t.Errorf("ExternalDepCerts expected empty, got %v", got)
	}
}

// The Argo-synced llz-letsencrypt-* ClusterIssuers sit Ready=False until the
// operator provisions dns.acmeEmail + LINODE_DNS_TOKEN — a supported deferred
// state that must classify Deferred, not Fail. Any other ClusterIssuer still
// hard-fails, so the deferral stays narrow.
func TestLetsencryptIssuersDeferred(t *testing.T) {
	extDep := ExternalDepIssuers()
	for _, name := range []string{"llz-letsencrypt-production", "llz-letsencrypt-staging"} {
		cat, _ := ClassifyReady("ClusterIssuer", name,
			"False", "ErrRegisterACMEAccount", "invalid contact domain", false, extDep)
		if cat != CatDeferred {
			t.Errorf("%s Ready=False = %v, want CatDeferred", name, cat)
		}
	}
	if _, ok := MatchExternalDep("platform-app-ca", extDep); ok {
		t.Error("an unrelated ClusterIssuer must NOT be deferred by the letsencrypt entry")
	}
}

// harbor-docker-config is deferred (not hard-failed) on a fresh bootstrap: it
// reads secret/harbor/robot, seeded by the harbor-robot-provisioner CronJob only
// after Harbor is up, so it sits Ready=False for the first few minutes. Any other
// ExternalSecret still hard-fails, so the deferral stays narrow.
func TestHarborDockerConfigDeferred(t *testing.T) {
	extDep := ExternalDepExternalSecrets()
	if _, ok := MatchExternalDep("llz-cert-automation/harbor-docker-config", extDep); !ok {
		t.Error("harbor-docker-config should be an operator-deferred ExternalSecret")
	}
	if _, ok := MatchExternalDep("harbor/harbor-core-secret", extDep); ok {
		t.Error("an unrelated ExternalSecret must NOT be deferred by the harbor-docker-config entry")
	}
	// A Ready=False SecretSyncedError on it must classify Deferred, not Fail.
	cat, _ := ClassifyReady("ExternalSecret", "llz-cert-automation/harbor-docker-config",
		"False", "SecretSyncedError", "could not get secret data from provider", false, extDep)
	if cat != CatDeferred {
		t.Errorf("harbor-docker-config Ready=False = %v, want CatDeferred", cat)
	}
}

// The llz-reconciler pod is deferred (not hard-failed) on a fresh bootstrap: its
// main container reads LINODE_TOKEN from the ESO-synced linode-api-token Secret,
// so it sits in CreateContainerConfigError for the first ~1-2 min after the
// openbao ClusterSecretStore goes Ready — until ESO first-syncs the Secret. The
// reconciler is a day-2 signal (health on its own metrics surface), not the
// bootstrap critical path, so it must not pin the convergence gate. The generated
// ReplicaSet/Pod suffix must still match; a pod that is not the reconciler (a
// different name, even in the same namespace) must not be swept in by the entry.
func TestLLZReconcilerPodDeferred(t *testing.T) {
	extDep := ExternalDepWorkloads()
	if _, ok := MatchExternalDep("llz-reconciler/llz-reconciler-5df46cbf66-64259", extDep); !ok {
		t.Error("the llz-reconciler pod (with a generated suffix) should be an operator-deferred workload")
	}
	if _, ok := MatchExternalDep("llz-reconciler/openbao-agent-injector-abc123", extDep); ok {
		t.Error("an unrelated pod in the llz-reconciler namespace must NOT be deferred by the reconciler entry")
	}
}

func TestNodeName(t *testing.T) {
	var n Node
	n.Metadata.Name = "lke-pool-abc"
	if n.Name() != "lke-pool-abc" {
		t.Errorf("Node.Name() = %q, want lke-pool-abc", n.Name())
	}
}
