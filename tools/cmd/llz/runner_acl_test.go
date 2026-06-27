package main

import (
	"context"
	"encoding/json"
	"errors"
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
	// clobberN simulates a racing writer: on each of the next clobberN PUTs the
	// fake overwrites our just-PUT list with clobberACL (as if another job's PUT
	// landed immediately after ours), exercising the verify-after-write retry.
	clobberN   int
	clobberACL linode.ControlPlaneACL
	// listErrs is consumed one-per-ListClusters-call to simulate transient
	// failures before a success — exercises listClustersWithRetry.
	listErrs []error
}

func (f *fakeACLClient) ListClusters(context.Context) ([]map[string]any, error) {
	if len(f.listErrs) > 0 {
		err := f.listErrs[0]
		f.listErrs = f.listErrs[1:]
		if err != nil {
			return nil, err
		}
	}
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
	if f.clobberN > 0 {
		f.clobberN--
		f.acl = f.clobberACL // a racing writer overwrote our PUT
	}
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
	// Zero the backoff so retry paths run instantly and deterministically (no
	// sleep, no RNG).
	prevDelay := aclRetryDelay
	aclRetryDelay = 0
	t.Cleanup(func() { newACLClient = prev; aclRetryDelay = prevDelay })
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

// A racing writer clobbers our open PUT once; verify-after-write must detect
// the dropped IP, re-read the racer's current list, and retry until it sticks.
func TestRunnerACLOpenRetriesWhenClobbered(t *testing.T) {
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}},
		clobberN:   1,
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"8.8.8.0/24"}}, // racer's list, sans our IP
	}
	withFakeACL(t, fake)

	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4", failOnMissing: true}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if len(fake.puts) != 2 {
		t.Fatalf("expected 2 PUTs (clobbered + retry), got %d: %+v", len(fake.puts), fake.puts)
	}
	if !fake.acl.ContainsIP("1.2.3.4") {
		t.Fatalf("final ACL must contain our IP after retry, got %+v", fake.acl)
	}
	if !fake.acl.ContainsIP("8.8.8.0/24") {
		t.Errorf("retry must preserve the racer's IP (re-read current list), got %+v", fake.acl)
	}
	if st, ok, _ := readRunnerACLState("e2e"); !ok || !st.Modified {
		t.Errorf("state should record Modified=true after a successful add, got ok=%v st=%+v", ok, st)
	}
}

// A writer that clobbers every PUT must eventually fail open (hard error so the
// job surfaces that this runner never got apiserver access) after the bounded
// retries, not loop forever.
func TestRunnerACLOpenFailsAfterMaxAttempts(t *testing.T) {
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}},
		clobberN:   1000, // always clobbered
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"8.8.8.0/24"}},
	}
	withFakeACL(t, fake)

	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4", failOnMissing: true}); err == nil {
		t.Fatal("expected open to fail after exhausting retries, got nil")
	}
	if len(fake.puts) != aclMaxAttempts {
		t.Errorf("expected %d PUT attempts, got %d", aclMaxAttempts, len(fake.puts))
	}
}

// A racing writer re-adds our IP after our revoke PUT; verify-after-write must
// detect it's still present and retry until the removal sticks.
func TestRunnerACLRevokeRetriesWhenReadded(t *testing.T) {
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32", "9.9.9.0/24"}},
		clobberN:   1,
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32", "9.9.9.0/24"}}, // racer re-added our IP
	}
	withFakeACL(t, fake)
	if err := writeRunnerACLState("e2e", runnerACLState{ClusterID: "5", IP: "1.2.3.4", Modified: true}); err != nil {
		t.Fatal(err)
	}
	if err := runRunnerACL("revoke", runnerACLOpts{region: "e2e"}); err != nil {
		t.Fatalf("revoke = %v", err)
	}
	if len(fake.puts) != 2 {
		t.Fatalf("expected 2 PUTs (clobbered + retry), got %d: %+v", len(fake.puts), fake.puts)
	}
	if fake.acl.ContainsIP("1.2.3.4") {
		t.Fatalf("final ACL must NOT contain our IP after revoke retry, got %+v", fake.acl)
	}
	if _, ok, _ := readRunnerACLState("e2e"); ok {
		t.Error("state file should be removed after a successful revoke")
	}
}

// Revoke runs under `if: always()`: a writer that keeps re-adding our IP must
// NOT make revoke return a hard error (that would fail an otherwise-green job).
func TestRunnerACLRevokeTolerantWhenAlwaysReadded(t *testing.T) {
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}},
		clobberN:   1000, // racer re-adds every time
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}},
	}
	withFakeACL(t, fake)
	if err := writeRunnerACLState("e2e", runnerACLState{ClusterID: "5", IP: "1.2.3.4", Modified: true}); err != nil {
		t.Fatal(err)
	}
	if err := runRunnerACL("revoke", runnerACLOpts{region: "e2e"}); err != nil {
		t.Fatalf("revoke must stay tolerant (nil) even when it can't win, got %v", err)
	}
	if len(fake.puts) != aclMaxAttempts {
		t.Errorf("expected %d PUT attempts before giving up, got %d", aclMaxAttempts, len(fake.puts))
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

// A transient transport blip on the cluster-list GET must be retried, not
// surfaced as a misleading "no cluster matched".
func TestResolveClusterIDRetriesTransientListFailure(t *testing.T) {
	fake := &fakeACLClient{
		clusters: []map[string]any{
			{"id": json.Number("4242"), "label": "lke-e2e", "region": "us-ord"},
		},
		listErrs: []error{errors.New("connection reset by peer"), errors.New("TLS handshake timeout")},
	}
	withFakeACL(t, fake) // zeroes aclRetryDelay so the retries are instant
	id, err := resolveClusterID(context.Background(), fake,
		clusterRef{clusterLabel: "lke-e2e", linodeRegion: "us-ord"})
	if err != nil {
		t.Fatalf("resolveClusterID should retry transient list failures: %v", err)
	}
	if id != 4242 {
		t.Errorf("resolved cluster id = %d, want 4242 (after 2 transient list failures)", id)
	}
	if len(fake.listErrs) != 0 {
		t.Errorf("%d simulated list failures left unconsumed — retry stopped early", len(fake.listErrs))
	}
}
