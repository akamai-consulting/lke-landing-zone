package teardown

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	detached  []uint64
}

func (f *fakeTeardownClient) DetachVolume(_ context.Context, id uint64) error {
	f.detached = append(f.detached, id)
	return nil
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
	// Model async success: a deleted cluster leaves the account, so a later
	// ClustersWithLabel no longer returns it — this is what drives
	// forceDeleteCluster's verify loop to completion (a delete that errors above
	// leaves the cluster in place, modelling a wedged cluster that won't die).
	const cp = "/v4beta/lke/clusters/"
	if strings.HasPrefix(path, cp) {
		if id, err := strconv.ParseUint(strings.TrimPrefix(path, cp), 10, 64); err == nil {
			f.clusters = removeUint(f.clusters, id)
		}
	}
	return nil
}

func removeUint(xs []uint64, drop uint64) []uint64 {
	out := xs[:0:0]
	for _, x := range xs {
		if x != drop {
			out = append(out, x)
		}
	}
	return out
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
	prevSleep := teardownSleep
	teardownSleep = func(time.Duration) {} // force-delete retries don't wait in tests
	t.Cleanup(func() { newTeardownClient = prev; teardownSleep = prevSleep })
	return dir, ghaEnv
}

// stubTerraformOutputs returns Deps whose Exec answers `terraform output -raw
// <name>` from the map (a missing name errors, like a real absent output) and
// rejects everything else.
//
// It RETURNS Deps rather than swapping a package-level var, which is the whole
// difference the extraction made: the seam is a parameter, so two tests can hold
// different stubs at once and neither has to clean up after itself.
func stubTerraformOutputs(t *testing.T, outputs map[string]string) Deps {
	t.Helper()
	d := testDeps(t)
	d.Exec = func(name string, args ...string) ([]byte, error) {
		if name != d.TFBin() || len(args) != 4 || args[1] != "output" {
			return nil, errors.New("unexpected command")
		}
		if v, ok := outputs[args[3]]; ok {
			return []byte(v + "\n"), nil
		}
		return nil, errors.New("no such output")
	}
	return d
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
	d := testDeps(t)
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
			{"id": float64(3), "label": "data-c", "linode_id": float64(11)},  // operator-owned: no platform prefix
			{"id": float64(4), "label": "pvc-ddd", "linode_id": nil},         // already detached
			// RELABELED by the volume-labels reconciler. The tracker used to hardcode
			// a `pvc-` prefix and skip this, so it was never handed to the sweep and
			// outlived the cluster — then squatted its label (Linode labels are
			// account-unique) and blocked the next cluster from relabeling.
			{"id": float64(5), "label": "e2e-harbor-harbor-otomi-db-1", "linode_id": float64(12)},
			// DETACHED at capture (pod mid-reschedule) but carrying this cluster's
			// lke<id> tag. Attachment is a point-in-time property, so keying on it
			// alone dropped this Volume and it survived its own cluster — observed
			// with pvc-0f8efbcdf6704500 on the lke638015 destroy. The tag has no
			// such window, so it is tracked regardless of attachment.
			{"id": float64(6), "label": "pvc-eee", "linode_id": nil, "tags": []any{"block-storage", "lke777"}},
			// Same tag, ALSO already renamed — both leak paths at once.
			{"id": float64(7), "label": "e2e-monitoring-storage-loki-0", "linode_id": nil, "tags": []any{"lke777"}},
			// Another cluster's tagged Volume must NOT be swept by this destroy.
			{"id": float64(8), "label": "pvc-fff", "linode_id": nil, "tags": []any{"lke999"}},
		},
	}
	dir, ghaEnv := withTeardown(t, fake, teardownTFVars)
	if err := RunCapture(d, "e2e", dir); err != nil {
		t.Fatalf("capture: %v", err)
	}
	got, _ := os.ReadFile(ghaEnv)
	want := "LKE_CLUSTER_ID=777\nCLUSTER_PVC_VOLUME_IDS=1 5 6 7\n"
	if string(got) != want {
		t.Errorf("GITHUB_ENV = %q, want %q", got, want)
	}
}

func TestTeardownCaptureClusterAlreadyGone(t *testing.T) {
	d := testDeps(t)
	fake := &fakeTeardownClient{}
	dir, ghaEnv := withTeardown(t, fake, teardownTFVars)
	if err := RunCapture(d, "e2e", dir); err != nil {
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
	d := stubTerraformOutputs(t, map[string]string{}) // no outputs in state

	// --yes deletes the cluster and the label-resolved firewall.
	if err := RunForceDelete(d, "e2e", dir); err != nil {
		t.Fatalf("force-delete: %v", err)
	}
	want := []string{"/v4beta/lke/clusters/777", "/v4/networking/firewalls/42"}
	if strings.Join(fake.deletes, " ") != strings.Join(want, " ") {
		t.Errorf("deletes = %v, want %v", fake.deletes, want)
	}

	// A failed delete warns but does not error (always()-path cleanup).
	fake.deletes = nil
	fake.deleteErr = map[string]error{"/v4beta/lke/clusters/777": errors.New("boom")}
	if err := RunForceDelete(d, "e2e", dir); err != nil {
		t.Errorf("force-delete with failing delete should warn, not error: %v", err)
	}
}

func TestTeardownForceDeletePrefersExactIDOutput(t *testing.T) {
	fake := &fakeTeardownClient{
		firewalls: []map[string]any{{"id": float64(42), "label": "e2e-lke-nodes"}},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{"node_firewall_id": "9001"})
	if err := RunForceDelete(d, "e2e", dir); err != nil {
		t.Fatalf("force-delete: %v", err)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/v4/networking/firewalls/9001" {
		t.Errorf("deletes = %v, want the exact-id firewall only", fake.deletes)
	}
}

func TestTeardownForceDeleteDryRun(t *testing.T) {
	fake := &fakeTeardownClient{clusters: []uint64{777}}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{})
	// Opt OUT of Confirm: the fixture defaults it to true so the destructive path
	// is what a test exercises unless it says otherwise. This is the one case that
	// wants the other branch, and it has to say so.
	d.Confirm = func() bool { return false }
	if err := RunForceDelete(d, "e2e", dir); err != nil {
		t.Fatalf("force-delete dry-run: %v", err)
	}
	if len(fake.deletes) != 0 {
		t.Errorf("dry-run must delete nothing, got %v", fake.deletes)
	}
}

// A wedged cluster whose DELETE keeps failing is re-issued the DELETE across the
// whole retry budget (the verify never sees it leave), then warns rather than
// erroring — the always()-path contract; assert-no-orphans is the hard gate.
func TestTeardownForceDeleteWedgedClusterRetriesThenWarns(t *testing.T) {
	fake := &fakeTeardownClient{
		clusters:  []uint64{777},
		deleteErr: map[string]error{"/v4beta/lke/clusters/777": errors.New("cluster stuck deleting")},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{})

	prevA := forceDeleteClusterAttempts
	forceDeleteClusterAttempts = 3
	t.Cleanup(func() { forceDeleteClusterAttempts = prevA })

	if err := RunForceDelete(d, "e2e", dir); err != nil {
		t.Fatalf("wedged cluster must warn, not error (always()-path cleanup): %v", err)
	}
	got := 0
	for _, p := range fake.deletes {
		if p == "/v4beta/lke/clusters/777" {
			got++
		}
	}
	if got != 3 {
		t.Errorf("wedged cluster: %d DELETE attempts, want 3 (the retry budget)", got)
	}
}

func TestClusterIDPresent(t *testing.T) {
	clusters := []map[string]any{
		{"id": float64(632652), "label": "instance-template-e2e"},
		{"id": float64(11), "label": "other"},
	}
	if !clusterIDPresent(clusters, 632652) {
		t.Error("632652 present but not detected")
	}
	if clusterIDPresent(clusters, 999) {
		t.Error("999 absent but reported present")
	}
	if clusterIDPresent(clusters, 0) {
		t.Error("id 0 must never match a real cluster")
	}
	if clusterIDPresent(nil, 632652) {
		t.Error("empty cluster list must not match")
	}
}

func TestTeardownDeleteVPC(t *testing.T) {
	// Resolved by label when the vpc_id output is absent.
	fake := &fakeTeardownClient{
		vpcs: []map[string]any{{"id": float64(55), "label": "e2e-lke-vpc"}},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{})
	if err := RunDeleteVPC(d, "e2e", dir, "", 3, 0, false); err != nil {
		t.Fatalf("delete-vpc: %v", err)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/v4/vpcs/55" {
		t.Errorf("deletes = %v, want /v4/vpcs/55", fake.deletes)
	}

	// In-use 409s retry up to --attempts, then warn without failing.
	fake.deletes = nil
	fake.deleteErr = map[string]error{"/v4/vpcs/55": errors.New("409 in use")}
	if err := RunDeleteVPC(d, "e2e", dir, "", 3, 0, false); err != nil {
		t.Errorf("exhausted retries should warn, not error: %v", err)
	}
	if len(fake.deletes) != 3 {
		t.Errorf("delete attempts = %d, want 3", len(fake.deletes))
	}

	// --require-deleted turns exhausted retries into a hard failure.
	fake.deletes = nil
	if err := RunDeleteVPC(d, "e2e", dir, "", 3, 0, true); err == nil {
		t.Error("--require-deleted should fail when the VPC is still undeletable")
	}

	// VPC gone entirely → clean no-op.
	fake.vpcs, fake.deletes = nil, nil
	if err := RunDeleteVPC(d, "e2e", dir, "", 3, 0, false); err != nil || len(fake.deletes) != 0 {
		t.Errorf("absent VPC should no-op (err=%v deletes=%v)", err, fake.deletes)
	}

	// The LKE-E auto VPC labeled lke<id> is resolved from the LKE_CLUSTER_ID env
	// (no --cluster-id flag passed) — the skew-safe path the workflow relies on.
	t.Setenv("LKE_CLUSTER_ID", "616722")
	fake.vpcs = []map[string]any{{"id": float64(77), "label": "lke616722"}}
	fake.deletes = nil
	if err := RunDeleteVPC(d, "e2e", dir, "", 3, 0, false); err != nil {
		t.Fatalf("delete-vpc via env cluster id: %v", err)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "/v4/vpcs/77" {
		t.Errorf("env-resolved lke616722 VPC: deletes = %v, want /v4/vpcs/77", fake.deletes)
	}
}

func TestScanOrphans(t *testing.T) {
	// live cluster 100 is kept; everything tagged/labelled for the gone 999 is
	// an orphan. One attached pvc Volume and one non-pvc Volume are ignored.
	fake := &fakeOrphanScanner{
		live: map[string]bool{"100": true},
		volumes: []map[string]any{
			{"id": float64(1), "label": "pvc-orphan", "region": "us-ord", "linode_id": nil},          // orphan
			{"id": float64(2), "label": "pvc-attached", "region": "us-ord", "linode_id": float64(5)}, // attached → keep
			{"id": float64(3), "label": "data-vol", "region": "us-ord", "linode_id": nil},            // not pvc-* → keep
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
	scan, err := ScanOrphans(context.Background(), fake, "", "", "")
	if err != nil {
		t.Fatalf("scanOrphans: %v", err)
	}
	if scan.LiveClusters != 1 {
		t.Errorf("liveClusters = %d, want 1", scan.LiveClusters)
	}
	if scan.Vol.Orphan != 1 || scan.NB.Orphan != 1 || scan.VPC.Orphan != 1 {
		t.Errorf("orphan counts = vol %d / nb %d / vpc %d, want 1/1/1", scan.Vol.Orphan, scan.NB.Orphan, scan.VPC.Orphan)
	}
	if scan.Orphans() != 3 {
		t.Errorf("orphans() = %d, want 3", scan.Orphans())
	}

	// The Volume orphan lives in us-ord; scoping Volumes to a DIFFERENT region
	// drops it while NB/VPC (account-wide here) still count. This is the
	// preflight-vs-reap alignment fix: a detached pvc-* Volume in another region
	// must not be counted against a us-ord apply that `llz reap --region us-ord`
	// would never clean.
	volElsewhere, err := ScanOrphans(context.Background(), fake, "", "us-east", "")
	if err != nil {
		t.Fatalf("ScanOrphans(volumeRegion): %v", err)
	}
	if volElsewhere.Vol.Orphan != 0 {
		t.Errorf("volume orphan scoped to us-east = %d, want 0 (the orphan is in us-ord)", volElsewhere.Vol.Orphan)
	}
	if volElsewhere.NB.Orphan != 1 || volElsewhere.VPC.Orphan != 1 {
		t.Errorf("NB/VPC should stay account-wide: nb %d / vpc %d, want 1/1", volElsewhere.NB.Orphan, volElsewhere.VPC.Orphan)
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
	scoped, err := ScanOrphans(context.Background(), fake, "us-ord", "us-ord", "")
	if err != nil {
		t.Fatalf("ScanOrphans(region): %v", err)
	}
	if scoped.Orphans() != 0 {
		t.Errorf("region-scoped orphans() = %d, want 0", scoped.Orphans())
	}
}

// settlingGateScanner answers the destroy gate's reads with an account that is
// still settling: the destroyed cluster's VPC stays visible for the first
// `vpcVisibleFor` reads of /v4/vpcs and is gone after that. Everything else is
// already clean, so the VPC is the only thing standing between the gate and a
// pass.
type settlingGateScanner struct {
	clusterID     string
	vpcVisibleFor int
	vpcReads      int
}

func (s *settlingGateScanner) LiveClusterIDs(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (s *settlingGateScanner) ListVolumes(context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (s *settlingGateScanner) ListNodeBalancers(context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (s *settlingGateScanner) NodeBalancerBackendCount(context.Context, uint64) (int, error) {
	return 0, nil
}
func (s *settlingGateScanner) ListClusters(context.Context) ([]map[string]any, error) {
	return nil, nil // the cluster itself is gone
}
func (s *settlingGateScanner) ListVPCs(context.Context) ([]map[string]any, error) {
	s.vpcReads++
	if s.vpcReads <= s.vpcVisibleFor {
		return []map[string]any{{"id": float64(587295), "label": "lke" + s.clusterID, "region": "us-ord"}}, nil
	}
	return nil, nil
}

// The gate's own-orphan check (cluster survived / own NBs / own VPC) reads the
// same asynchronously-reaped account state as the threshold census, so it needs
// the same retries. It used to run ONCE, ahead of the loop.
//
// Run 30643426633: `teardown-delete-vpc` printed "VPC 587295 deleted.", and this
// gate — invoked `--attempts 5 --retry-delay 30` — read the VPC list 5 seconds
// later, still saw lke638293, and failed the teardown 0.9s in. It rode out none
// of the settling window it documents, and reported a leak that was a delete
// still propagating.
func TestAssertNoOrphansRidesOutTheSettlingWindow(t *testing.T) {
	orig := teardownSleep
	teardownSleep = func(time.Duration) {}
	t.Cleanup(func() { teardownSleep = orig })

	t.Run("a VPC that disappears on the second read passes", func(t *testing.T) {
		s := &settlingGateScanner{clusterID: "638293", vpcVisibleFor: 1}
		out := captureStdout(t, func() {
			if err := assertNoOrphans(context.Background(), s, "", "e2e", "638293", "e2e", 0, 5, 30); err != nil {
				t.Fatalf("a VPC still listed on the FIRST read but gone on the second is a delete propagating, not a leak — the gate has 5 attempts precisely to let it: %v", err)
			}
		})
		if !strings.Contains(out, "scoped teardown is clean") {
			t.Errorf("the gate must announce the cluster clean once its VPC settles:\n%s", out)
		}
	})

	t.Run("a VPC that outlives every attempt still fails", func(t *testing.T) {
		s := &settlingGateScanner{clusterID: "638293", vpcVisibleFor: 99}
		err := assertNoOrphans(context.Background(), s, "", "e2e", "638293", "e2e", 0, 3, 30)
		if err == nil {
			t.Fatal("a VPC that survives ALL attempts is a real leak and must red the teardown — retrying must not become tolerating")
		}
		if !strings.Contains(err.Error(), "1 VPC of its own") {
			t.Errorf("the failure must still name what leaked; got %v", err)
		}
		if s.vpcReads != 3 {
			t.Errorf("VPC reads = %d, want 3 (one per attempt)", s.vpcReads)
		}
	})

	t.Run("a single attempt fails on the first read, as configured", func(t *testing.T) {
		// --attempts 1 is opting OUT of the settling window; the gate must honor
		// that rather than silently retrying anyway.
		s := &settlingGateScanner{clusterID: "638293", vpcVisibleFor: 1}
		if err := assertNoOrphans(context.Background(), s, "", "e2e", "638293", "e2e", 0, 1, 30); err == nil {
			t.Fatal("--attempts 1 must take exactly one reading")
		}
	})
}
