package health

import "testing"

// The operator-deferred external-dependency allowlists are currently empty (the
// repo ships none); pin that so a future non-empty entry is a deliberate edit,
// and cover the otherwise-unreferenced accessors.
func TestExternalDepAllowlistsEmpty(t *testing.T) {
	for name, got := range map[string][]DepEntry{
		"issuers":         ExternalDepIssuers(),
		"certs":           ExternalDepCerts(),
		"externalSecrets": ExternalDepExternalSecrets(),
	} {
		if len(got) != 0 {
			t.Errorf("ExternalDep%s expected empty, got %v", name, got)
		}
	}
}

func TestNodeName(t *testing.T) {
	var n Node
	n.Metadata.Name = "lke-pool-abc"
	if n.Name() != "lke-pool-abc" {
		t.Errorf("Node.Name() = %q, want lke-pool-abc", n.Name())
	}
}
