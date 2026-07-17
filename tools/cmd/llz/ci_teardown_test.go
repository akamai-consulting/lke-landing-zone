package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTeardownClient implements teardownClient from canned data, recording
// every DeleteResourcePath call.
type fakeTeardownClient struct {
	clusters  []uint64
	pools     []map[string]any
	volumes   []map[string]any
	firewalls []map[string]any
	vpcs      []map[string]any
	deleteErr map[string]error // per-path injected failure
	deletes   []string
}

func (f *fakeTeardownClient) ClustersWithLabel(context.Context, string) ([]uint64, error) {
	return f.clusters, nil
}
func (f *fakeTeardownClient) ListNodePools(context.Context, uint64) ([]map[string]any, error) {
	return f.pools, nil
}
func (f *fakeTeardownClient) ListVolumes(context.Context) ([]map[string]any, error) {
	return f.volumes, nil
}
func (f *fakeTeardownClient) ListFirewalls(context.Context) ([]map[string]any, error) {
	return f.firewalls, nil
}
func (f *fakeTeardownClient) ListVPCs(context.Context) ([]map[string]any, error) {
	return f.vpcs, nil
}
func (f *fakeTeardownClient) DeleteResourcePath(_ context.Context, path string) error {
	f.deletes = append(f.deletes, path)
	if err, ok := f.deleteErr[path]; ok {
		return err
	}
	return nil
}

// withTeardown wires the fake client, a tfvars dir, the token env, and a
// GITHUB_ENV capture file; returns (tfDir, ghaEnvPath).
func withTeardown(t *testing.T, fake *fakeTeardownClient, tfvars string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "e2e.tfvars"), []byte(tfvars), 0o644); err != nil {
		t.Fatal(err)
	}
	ghaEnv := filepath.Join(t.TempDir(), "gha-env")
	t.Setenv("GITHUB_ENV", ghaEnv)
	t.Setenv("LINODE_TOKEN", "tok")
	prev := newTeardownClient
	newTeardownClient = func(string) teardownClient { return fake }
	t.Cleanup(func() { newTeardownClient = prev })
	return dir, ghaEnv
}

// stubTerraformOutputs makes execOutput answer `terraform output -raw <name>`
// from the map (missing name errors, like a real absent output) and rejects
// everything else.
func stubTerraformOutputs(t *testing.T, outputs map[string]string) {
	t.Helper()
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "terraform" || len(args) != 4 || args[1] != "output" {
			return nil, errors.New("unexpected command")
		}
		if v, ok := outputs[args[3]]; ok {
			return []byte(v + "\n"), nil
		}
		return nil, errors.New("no such output")
	})
}

const teardownTFVars = "cluster_label = \"e2e-lke\"\n"

func TestNumericOrEmpty(t *testing.T) {
	for in, want := range map[string]string{
		"12345": "12345", "": "", "abc": "",
		"Warning: No outputs found": "", "12a": "",
	} {
		if got := numericOrEmpty(in); got != want {
			t.Errorf("numericOrEmpty(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTeardownCapture(t *testing.T) {
	fake := &fakeTeardownClient{
		clusters: []uint64{777},
		pools: []map[string]any{
			{"nodes": []any{
				map[string]any{"instance_id": float64(11)},
				map[string]any{"instance_id": float64(12)},
			}},
		},
		volumes: []map[string]any{
			{"id": float64(1), "label": "pvc-aaa", "linode_id": float64(11)}, // ours
			{"id": float64(2), "label": "pvc-bbb", "linode_id": float64(99)}, // peer's node
			{"id": float64(3), "label": "data-c", "linode_id": float64(11)},  // not pvc-*
			{"id": float64(4), "label": "pvc-ddd", "linode_id": nil},         // already detached
		},
	}
	dir, ghaEnv := withTeardown(t, fake, teardownTFVars)
	if err := runCITeardownCapture("e2e", dir); err != nil {
		t.Fatalf("capture: %v", err)
	}
	got, _ := os.ReadFile(ghaEnv)
	want := "LKE_CLUSTER_ID=777\nCLUSTER_PVC_VOLUME_IDS=1\n"
	if string(got) != want {
		t.Errorf("GITHUB_ENV = %q, want %q", got, want)
	}
}

func TestTeardownCaptureClusterAlreadyGone(t *testing.T) {
	fake := &fakeTeardownClient{}
	dir, ghaEnv := withTeardown(t, fake, teardownTFVars)
	if err := runCITeardownCapture("e2e", dir); err != nil {
		t.Fatalf("capture: %v", err)
	}
	got, _ := os.ReadFile(ghaEnv)
	// Keys still written (the sweeps' guards read them), values empty.
	if string(got) != "LKE_CLUSTER_ID=\nCLUSTER_PVC_VOLUME_IDS=\n" {
		t.Errorf("GITHUB_ENV = %q, want empty values", got)
	}
}

func TestTeardownForceDelete(t *testing.T) {
	fake := &fakeTeardownClient{
		clusters: []uint64{777},
		// The module-correct fallback label for cluster_label "e2e-lke" with no
		// firewall_label tfvars override (ResolveFirewallLabel).
		firewalls: []map[string]any{
			{"id": float64(42), "label": "e2e-lke-nodes"},
		},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	stubTerraformOutputs(t, map[string]string{}) // no outputs in state

	// --yes deletes the cluster and the label-resolved firewall.
	if err := runCITeardownForceDelete(globalOpts{yes: true}, "e2e", dir); err != nil {
		t.Fatalf("force-delete: %v", err)
	}
	want := []string{"/v4beta/lke/clusters/777", "/v4/networking/firewalls/42"}
	if strings.Join(fake.deletes, " ") != strings.Join(want, " ") {
		t.Errorf("deletes = %v, want %v", fake.deletes, want)
	}

	// A failed delete warns but does not error (always()-path cleanup).
	fake.deletes = nil
	fake.deleteErr = map[string]error{"/v4beta/lke/clusters/777": errors.New("boom")}
	if err := runCITeardownForceDelete(globalOpts{yes: true}, "e2e", dir); err != nil {
		t.Errorf("force-delete with failing delete should warn, not error: %v", err)
	}
}

func TestTeardownForceDeletePrefersExactIDOutput(t *testing.T) {
	fake := &fakeTeardownClient{
		firewalls: []map[string]any{{"id": float64(42), "label": "e2e-lke-nodes"}},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	stubTerraformOutputs(t, map[string]string{"node_firewall_id": "9001"})
	if err := runCITeardownForceDelete(globalOpts{yes: true}, "e2e", dir); err != nil {
		t.Fatalf("force-delete: %v", err)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/v4/networking/firewalls/9001" {
		t.Errorf("deletes = %v, want the exact-id firewall only", fake.deletes)
	}
}

func TestTeardownForceDeleteDryRun(t *testing.T) {
	fake := &fakeTeardownClient{clusters: []uint64{777}}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	stubTerraformOutputs(t, map[string]string{})
	if err := runCITeardownForceDelete(globalOpts{}, "e2e", dir); err != nil {
		t.Fatalf("force-delete dry-run: %v", err)
	}
	if len(fake.deletes) != 0 {
		t.Errorf("dry-run must delete nothing, got %v", fake.deletes)
	}
}

func TestTeardownDeleteVPC(t *testing.T) {
	// Resolved by label when the vpc_id output is absent.
	fake := &fakeTeardownClient{
		vpcs: []map[string]any{{"id": float64(55), "label": "e2e-lke-vpc"}},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	stubTerraformOutputs(t, map[string]string{})
	if err := runCITeardownDeleteVPC(globalOpts{yes: true}, "e2e", dir, "", 3, 0, false); err != nil {
		t.Fatalf("delete-vpc: %v", err)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/v4/vpcs/55" {
		t.Errorf("deletes = %v, want /v4/vpcs/55", fake.deletes)
	}

	// In-use 409s retry up to --attempts, then warn without failing.
	fake.deletes = nil
	fake.deleteErr = map[string]error{"/v4/vpcs/55": errors.New("409 in use")}
	if err := runCITeardownDeleteVPC(globalOpts{yes: true}, "e2e", dir, "", 3, 0, false); err != nil {
		t.Errorf("exhausted retries should warn, not error: %v", err)
	}
	if len(fake.deletes) != 3 {
		t.Errorf("delete attempts = %d, want 3", len(fake.deletes))
	}

	// --require-deleted turns exhausted retries into a hard failure.
	fake.deletes = nil
	if err := runCITeardownDeleteVPC(globalOpts{yes: true}, "e2e", dir, "", 3, 0, true); err == nil {
		t.Error("--require-deleted should fail when the VPC is still undeletable")
	}

	// VPC gone entirely → clean no-op.
	fake.vpcs, fake.deletes = nil, nil
	if err := runCITeardownDeleteVPC(globalOpts{yes: true}, "e2e", dir, "", 3, 0, false); err != nil || len(fake.deletes) != 0 {
		t.Errorf("absent VPC should no-op (err=%v deletes=%v)", err, fake.deletes)
	}

	// The LKE-E auto VPC labeled lke<id> is resolved from the LKE_CLUSTER_ID env
	// (no --cluster-id flag passed) — the skew-safe path the workflow relies on.
	t.Setenv("LKE_CLUSTER_ID", "616722")
	fake.vpcs = []map[string]any{{"id": float64(77), "label": "lke616722"}}
	fake.deletes = nil
	if err := runCITeardownDeleteVPC(globalOpts{yes: true}, "e2e", dir, "", 3, 0, false); err != nil {
		t.Fatalf("delete-vpc via env cluster id: %v", err)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/v4/vpcs/77" {
		t.Errorf("env-resolved lke616722 VPC: deletes = %v, want /v4/vpcs/77", fake.deletes)
	}
}

func TestWaitVolumesDetached(t *testing.T) {
	// Already detached → returns without sleeping.
	fake := &fakeTeardownClient{volumes: []map[string]any{
		{"id": float64(1), "label": "pvc-a", "linode_id": nil},
	}}
	waitVolumesDetached(context.Background(), fake, "1", 600)

	// Still attached + zero budget → gives up after the immediate check.
	fake.volumes = []map[string]any{{"id": float64(1), "label": "pvc-a", "linode_id": float64(7)}}
	waitVolumesDetached(context.Background(), fake, "1", 0)
}

// fakeOrphanScanner implements orphanScanner from canned data.
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

func TestScanOrphans(t *testing.T) {
	// live cluster 100 is kept; everything tagged/labelled for the gone 999 is a
	// DEFINITIVE orphan. An untagged detached pvc- Volume is counted SEPARATELY
	// (vol.untagged) — not a gone-cluster orphan, so it never gates. One attached
	// pvc Volume and one non-pvc Volume are ignored.
	fake := &fakeOrphanScanner{
		live: map[string]bool{"100": true},
		volumes: []map[string]any{
			{"id": float64(1), "label": "pvc-gone", "region": "us-ord", "linode_id": nil, "tags": []any{"lke999"}}, // gone cluster → orphan
			{"id": float64(4), "label": "pvc-untagged", "region": "us-ord", "linode_id": nil},                      // no lke tag → untagged (not gated)
			{"id": float64(2), "label": "pvc-attached", "region": "us-ord", "linode_id": float64(5)},               // attached → keep
			{"id": float64(3), "label": "data-vol", "region": "us-ord", "linode_id": nil},                          // not pvc-* → keep
		},
		nbs: []map[string]any{
			{"id": float64(10), "label": "nb-gone", "region": "us-ord", "tags": []any{"lke999"}}, // orphan (cluster gone)
			{"id": float64(11), "label": "nb-live", "region": "us-ord", "tags": []any{"lke100"}}, // keep (cluster live)
		},
		vpcs: []map[string]any{
			{"id": float64(20), "label": "lke999", "region": "us-ord"}, // orphan
			{"id": float64(21), "label": "lke100", "region": "us-ord"}, // keep
		},
	}
	scan, err := scanOrphans(context.Background(), fake, "", "")
	if err != nil {
		t.Fatalf("scanOrphans: %v", err)
	}
	if scan.liveClusters != 1 {
		t.Errorf("liveClusters = %d, want 1", scan.liveClusters)
	}
	if scan.vol.orphan != 1 || scan.nb.orphan != 1 || scan.vpc.orphan != 1 {
		t.Errorf("orphan counts = vol %d / nb %d / vpc %d, want 1/1/1", scan.vol.orphan, scan.nb.orphan, scan.vpc.orphan)
	}
	// The untagged detached Volume is counted separately and does NOT gate.
	if scan.vol.untagged != 1 {
		t.Errorf("vol.untagged = %d, want 1 (the untagged detached pvc- Volume)", scan.vol.untagged)
	}
	if scan.orphans() != 3 {
		t.Errorf("orphans() = %d, want 3 (gone-cluster Volume + NB + VPC; untagged excluded)", scan.orphans())
	}

	// The Volume orphan lives in us-ord; scoping Volumes to a DIFFERENT region
	// drops it while NB/VPC (account-wide here) still count. This is the
	// preflight-vs-reap alignment fix: a detached pvc-* Volume in another region
	// must not be counted against a us-ord apply that `llz reap --region us-ord`
	// would never clean.
	volElsewhere, err := scanOrphans(context.Background(), fake, "", "us-east")
	if err != nil {
		t.Fatalf("scanOrphans(volumeRegion): %v", err)
	}
	if volElsewhere.vol.orphan != 0 {
		t.Errorf("volume orphan scoped to us-east = %d, want 0 (the orphan is in us-ord)", volElsewhere.vol.orphan)
	}
	if volElsewhere.nb.orphan != 1 || volElsewhere.vpc.orphan != 1 {
		t.Errorf("NB/VPC should stay account-wide: nb %d / vpc %d, want 1/1", volElsewhere.nb.orphan, volElsewhere.vpc.orphan)
	}

	// Region filter excludes the orphans parked in another region.
	for _, m := range fake.volumes {
		m["region"] = "us-east"
	}
	for _, m := range fake.nbs {
		m["region"] = "us-east"
	}
	for _, m := range fake.vpcs {
		m["region"] = "us-east"
	}
	scoped, err := scanOrphans(context.Background(), fake, "us-ord", "us-ord")
	if err != nil {
		t.Fatalf("scanOrphans(region): %v", err)
	}
	if scoped.orphans() != 0 {
		t.Errorf("region-scoped orphans() = %d, want 0", scoped.orphans())
	}
}
