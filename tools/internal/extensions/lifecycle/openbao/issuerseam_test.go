package openbao

import "testing"

// withIssuerDiscovery installs the cluster-issuer lookup for one test.
func withIssuerDiscovery(t *testing.T, fn func() string) {
	t.Helper()
	orig := DiscoverIssuerFromCluster
	DiscoverIssuerFromCluster = fn
	t.Cleanup(func() { DiscoverIssuerFromCluster = orig })
}
