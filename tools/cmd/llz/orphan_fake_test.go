package main

// orphan_fake_test.go — the canned OrphanScanner both sides need.
//
// A DUPLICATE of the copy in internal/teardown, and deliberately so: a test
// fixture cannot cross a package boundary, and the alternative — exporting it
// from production code, or minting a testhelpers package — is worse than two
// twenty-line fakes over an interface that is itself the contract. The interface
// is what keeps them honest; if it changes, both stop compiling.

import "context"

// fakeOrphanScanner implements OrphanScanner from canned data.
type fakeOrphanScanner struct {
	live     map[string]bool
	volumes  []map[string]any
	nbs      []map[string]any
	vpcs     []map[string]any
	backends map[uint64]int
}

func (f *fakeOrphanScanner) LiveClusterIDs(context.Context) (map[string]bool, error) {
	return f.live, nil
}
func (f *fakeOrphanScanner) ListVolumes(context.Context) ([]map[string]any, error) {
	return f.volumes, nil
}
func (f *fakeOrphanScanner) ListNodeBalancers(context.Context) ([]map[string]any, error) {
	return f.nbs, nil
}
func (f *fakeOrphanScanner) NodeBalancerBackendCount(_ context.Context, id uint64) (int, error) {
	return f.backends[id], nil
}
func (f *fakeOrphanScanner) ListVPCs(context.Context) ([]map[string]any, error) {
	return f.vpcs, nil
}
