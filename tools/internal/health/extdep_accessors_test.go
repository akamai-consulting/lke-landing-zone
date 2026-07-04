package health

import "testing"

// The Issuer/Cert operator-deferred allowlists ship empty; pin that so a future
// non-empty entry is a deliberate edit, and cover the otherwise-unreferenced
// accessors.
func TestExternalDepAllowlistsEmpty(t *testing.T) {
	for name, got := range map[string][]DepEntry{
		"issuers": ExternalDepIssuers(),
		"certs":   ExternalDepCerts(),
	} {
		if len(got) != 0 {
			t.Errorf("ExternalDep%s expected empty, got %v", name, got)
		}
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

func TestNodeName(t *testing.T) {
	var n Node
	n.Metadata.Name = "lke-pool-abc"
	if n.Name() != "lke-pool-abc" {
		t.Errorf("Node.Name() = %q, want lke-pool-abc", n.Name())
	}
}
