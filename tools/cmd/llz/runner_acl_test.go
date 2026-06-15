package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

type fakeACLClient struct {
	clusters []map[string]any
	acl      linode.ControlPlaneACL
	getErr   error
	putErr   error
	puts     []linode.ControlPlaneACL
}

func (f *fakeACLClient) ListClusters(context.Context) ([]map[string]any, error) {
	return f.clusters, nil
}
func (f *fakeACLClient) GetControlPlaneACL(context.Context, uint64) (linode.ControlPlaneACL, error) {
	return f.acl, f.getErr
}
func (f *fakeACLClient) PutControlPlaneACL(_ context.Context, _ uint64, a linode.ControlPlaneACL) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, a)
	f.acl = a
	return nil
}

// withFakeACL points the command's seams at fake and a hermetic state dir.
func withFakeACL(t *testing.T, fake *fakeACLClient) {
	t.Helper()
	t.Setenv("RUNNER_TEMP", t.TempDir())
	t.Setenv("LINODE_TOKEN", "tok")
	t.Setenv("LINODE_API_TOKEN", "")
	prev := newACLClient
	newACLClient = func(string) aclClient { return fake }
	t.Cleanup(func() { newACLClient = prev })
}

func TestRunnerACLOpenAddsIPAndRecordsState(t *testing.T) {
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}}}
	withFakeACL(t, fake)

	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4", failOnMissing: true}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if len(fake.puts) != 1 || !fake.puts[0].ContainsIP("1.2.3.4") {
		t.Fatalf("expected one PUT adding 1.2.3.4, got %+v", fake.puts)
	}
	st, ok, err := readRunnerACLState("e2e")
	if err != nil || !ok {
		t.Fatalf("state not written: ok=%v err=%v", ok, err)
	}
	if st.ClusterID != "5" || st.IP != "1.2.3.4" || !st.Modified {
		t.Errorf("state = %+v, want {5 1.2.3.4 true}", st)
	}
}

func TestRunnerACLOpenNoChangeWhenPresent(t *testing.T) {
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}}}
	withFakeACL(t, fake)

	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4"}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if len(fake.puts) != 0 {
		t.Errorf("expected no PUT when IP already present, got %+v", fake.puts)
	}
	st, _, _ := readRunnerACLState("e2e")
	if st.Modified {
		t.Error("state Modified = true, want false (no change)")
	}
}

func TestRunnerACLOpenNoChangeWhenACLDisabled(t *testing.T) {
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: false}}
	withFakeACL(t, fake)

	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4"}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if len(fake.puts) != 0 {
		t.Errorf("expected no PUT when ACL disabled, got %+v", fake.puts)
	}
}

func TestRunnerACLRevokeRemovesIPAndClearsState(t *testing.T) {
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32", "9.9.9.0/24"}}}
	withFakeACL(t, fake)

	// Seed the state file as a prior open(modified) would.
	if err := writeRunnerACLState("e2e", runnerACLState{ClusterID: "5", IP: "1.2.3.4", Modified: true}); err != nil {
		t.Fatal(err)
	}
	if err := runRunnerACL("revoke", runnerACLOpts{region: "e2e"}); err != nil {
		t.Fatalf("revoke = %v", err)
	}
	if len(fake.puts) != 1 || fake.puts[0].ContainsIP("1.2.3.4") {
		t.Fatalf("expected one PUT removing 1.2.3.4, got %+v", fake.puts)
	}
	if _, ok, _ := readRunnerACLState("e2e"); ok {
		t.Error("state file should be removed after revoke")
	}
}

func TestRunnerACLRevokeNoStateIsNoOp(t *testing.T) {
	fake := &fakeACLClient{}
	withFakeACL(t, fake)
	if err := runRunnerACL("revoke", runnerACLOpts{region: "absent"}); err != nil {
		t.Fatalf("revoke(no state) = %v", err)
	}
	if len(fake.puts) != 0 {
		t.Error("revoke with no state should not PUT")
	}
}

func TestRunnerACLEmptyTokenNoOps(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	// newACLClient must not even be called; leave the default in place.
	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4"}); err != nil {
		t.Errorf("empty-token open should no-op nil, got %v", err)
	}
}

func TestRunnerACLOpenUnresolvableTolerated(t *testing.T) {
	fake := &fakeACLClient{} // no clusters → resolution fails
	withFakeACL(t, fake)
	// fail-on-missing=false → no-op (e.g. a destroy job with no cluster).
	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterLabel: "gone", failOnMissing: false}); err != nil {
		t.Errorf("unresolvable open with fail-on-missing=false should no-op, got %v", err)
	}
	// fail-on-missing=true → error.
	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterLabel: "gone", failOnMissing: true}); err == nil {
		t.Error("unresolvable open with fail-on-missing=true should error")
	}
}

func TestResolveClusterIDFromTFVars(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "e2e.tfvars"),
		[]byte("cluster_label = \"lke-e2e\"\nregion = \"us-ord\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeACLClient{clusters: []map[string]any{
		{"id": json.Number("7"), "label": "lke-e2e", "region": "us-ord"},
		{"id": json.Number("8"), "label": "lke-e2e", "region": "us-sea"},
	}}
	id, err := resolveClusterID(context.Background(), fake, clusterRef{region: "e2e", tfvarsDir: dir})
	if err != nil {
		t.Fatalf("resolveClusterID = %v", err)
	}
	if id != 7 {
		t.Errorf("resolved cluster id = %d, want 7 (label+region from tfvars)", id)
	}
}
