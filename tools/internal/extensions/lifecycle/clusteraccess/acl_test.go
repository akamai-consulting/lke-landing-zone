package clusteraccess

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

const fakeACLRevision = "rev-test"

type fakeACLClient struct {
	clusters []map[string]any
	acl      linode.ControlPlaneACL
	getErr   error
	putErr   error
	puts     []linode.ControlPlaneACL
	// gets counts ACL reads, so a test can assert a read-only path still reads.
	gets int
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
	f.gets++
	return f.acl, f.getErr
}
func (f *fakeACLClient) PutControlPlaneACL(_ context.Context, _ uint64, a linode.ControlPlaneACL) (string, error) {
	if f.putErr != nil {
		return "", f.putErr
	}
	f.puts = append(f.puts, a)
	f.acl = a
	// Enforcement is reported IMMEDIATELY here so the existing cases keep
	// exercising the happy path. The enforcement WAIT is covered separately by
	// TestWaitACLEnforced, which is where the async behaviour belongs.
	f.acl.RevisionID = fakeACLRevision
	if f.clobberN > 0 {
		f.clobberN--
		f.acl = f.clobberACL // a racing writer overwrote our PUT
	}
	return fakeACLRevision, nil
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
	d := testDeps(t)
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}}}
	withFakeACL(t, fake)

	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4", FailOnMissing: true}); err != nil {
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
	d := testDeps(t)
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}}}
	withFakeACL(t, fake)

	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4"}); err != nil {
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
	d := testDeps(t)
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: false}}
	withFakeACL(t, fake)

	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4"}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if len(fake.puts) != 0 {
		t.Errorf("expected no PUT when ACL disabled, got %+v", fake.puts)
	}
}

func TestRunnerACLRevokeRemovesIPAndClearsState(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32", "9.9.9.0/24"}}}
	withFakeACL(t, fake)

	// Seed the state file as a prior open(modified) would.
	if err := writeRunnerACLState("e2e", runnerACLState{ClusterID: "5", IP: "1.2.3.4", Modified: true}); err != nil {
		t.Fatal(err)
	}
	if err := RunACL(d, "revoke", ACLOpts{Region: "e2e"}); err != nil {
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
	d := testDeps(t)
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}},
		clobberN:   1,
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"8.8.8.0/24"}}, // racer's list, sans our IP
	}
	withFakeACL(t, fake)

	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4", FailOnMissing: true}); err != nil {
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
	d := testDeps(t)
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}},
		clobberN:   1000, // always clobbered
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"8.8.8.0/24"}},
	}
	withFakeACL(t, fake)

	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4", FailOnMissing: true}); err == nil {
		t.Fatal("expected open to fail after exhausting retries, got nil")
	}
	if len(fake.puts) != aclMaxAttempts {
		t.Errorf("expected %d PUT attempts, got %d", aclMaxAttempts, len(fake.puts))
	}
}

// A racing writer re-adds our IP after our revoke PUT; verify-after-write must
// detect it's still present and retry until the removal sticks.
func TestRunnerACLRevokeRetriesWhenReadded(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32", "9.9.9.0/24"}},
		clobberN:   1,
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32", "9.9.9.0/24"}}, // racer re-added our IP
	}
	withFakeACL(t, fake)
	if err := writeRunnerACLState("e2e", runnerACLState{ClusterID: "5", IP: "1.2.3.4", Modified: true}); err != nil {
		t.Fatal(err)
	}
	if err := RunACL(d, "revoke", ACLOpts{Region: "e2e"}); err != nil {
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
// NOT make revoke return a hard error (that would fail an otherwise-color.Green job).
func TestRunnerACLRevokeTolerantWhenAlwaysReadded(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{
		acl:        linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}},
		clobberN:   1000, // racer re-adds every time
		clobberACL: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}},
	}
	withFakeACL(t, fake)
	if err := writeRunnerACLState("e2e", runnerACLState{ClusterID: "5", IP: "1.2.3.4", Modified: true}); err != nil {
		t.Fatal(err)
	}
	if err := RunACL(d, "revoke", ACLOpts{Region: "e2e"}); err != nil {
		t.Fatalf("revoke must stay tolerant (nil) even when it can't win, got %v", err)
	}
	if len(fake.puts) != aclMaxAttempts {
		t.Errorf("expected %d PUT attempts before giving up, got %d", aclMaxAttempts, len(fake.puts))
	}
}

func TestRunnerACLRevokeNoStateIsNoOp(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{}
	withFakeACL(t, fake)
	if err := RunACL(d, "revoke", ACLOpts{Region: "absent"}); err != nil {
		t.Fatalf("revoke(no state) = %v", err)
	}
	if len(fake.puts) != 0 {
		t.Error("revoke with no state should not PUT")
	}
}

func TestRunnerACLEmptyTokenNoOps(t *testing.T) {
	d := testDeps(t)
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	// newACLClient must not even be called; leave the default in place.
	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4"}); err != nil {
		t.Errorf("empty-token open should no-op nil, got %v", err)
	}
}

func TestRunnerACLOpenUnresolvableTolerated(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{} // no clusters → resolution fails
	withFakeACL(t, fake)
	// fail-on-missing=false → no-op (e.g. a destroy job with no cluster).
	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterLabel: "gone", FailOnMissing: false}); err != nil {
		t.Errorf("unresolvable open with fail-on-missing=false should no-op, got %v", err)
	}
	// fail-on-missing=true → error.
	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterLabel: "gone", FailOnMissing: true}); err == nil {
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
	id, err := resolveClusterID(context.Background(), fake, ClusterRef{Region: "e2e", TfvarsDir: dir})
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
		ClusterRef{ClusterLabel: "lke-e2e", LinodeRegion: "us-ord"})
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

// enforceFake reports a revision as enforced only after N GETs, modelling the
// asynchronous enforcement the API documents ("up to 20 minutes").
type enforceFake struct {
	revision   string
	afterGets  int
	gets       int
	getErrOnce bool
}

func (f *enforceFake) GetControlPlaneACL(context.Context, uint64) (linode.ControlPlaneACL, error) {
	f.gets++
	if f.getErrOnce && f.gets == 1 {
		return linode.ControlPlaneACL{}, errString("temporary API error")
	}
	acl := linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}}
	if f.gets >= f.afterGets {
		acl.RevisionID = f.revision // control plane has now verified enforcement
	} else {
		acl.RevisionID = "older-revision"
	}
	return acl, nil
}
func (f *enforceFake) PutControlPlaneACL(context.Context, uint64, linode.ControlPlaneACL) (string, error) {
	return f.revision, nil
}
func (f *enforceFake) ListClusters(context.Context) ([]map[string]any, error) { return nil, nil }

// THE BUG THIS CLOSES. PutControlPlaneACL is asynchronous: a GET straight after it
// echoes the DESIRED address list, so the old ContainsIP verify proved only that
// the API accepted the write. cluster-access announced "added <ip> to
// control-plane ACL" and moved on, and every kubectl then timed out or EOF'd
// until enforcement caught up — passing when enforcement was fast (run 7, 48s)
// and failing when it was not (runs 8-12). The API's only enforcement signal is
// the submitted revision-id appearing on GET.
func TestWaitACLEnforced(t *testing.T) {
	prevSleep := aclSleep
	aclSleep = func(time.Duration) {}
	t.Cleanup(func() { aclSleep = prevSleep })

	t.Run("waits until the revision is reflected", func(t *testing.T) {
		f := &enforceFake{revision: "rev-9", afterGets: 3}
		if err := waitACLEnforced(context.Background(), f, 1, "rev-9"); err != nil {
			t.Fatalf("want enforcement detected, got %v", err)
		}
		if f.gets < 3 {
			t.Errorf("returned after %d GET(s) — it accepted a STALE revision as enforced, "+
				"which is exactly the desired-state read that made this racy", f.gets)
		}
	})

	t.Run("a transient GET error does not abort the wait", func(t *testing.T) {
		f := &enforceFake{revision: "rev-9", afterGets: 2, getErrOnce: true}
		if err := waitACLEnforced(context.Background(), f, 1, "rev-9"); err != nil {
			t.Errorf("one API blip must not end the wait: %v", err)
		}
	})

	t.Run("times out rather than blocking the job forever", func(t *testing.T) {
		prev := aclEnforceWait
		aclEnforceWait = 0 // deadline already passed
		t.Cleanup(func() { aclEnforceWait = prev })
		f := &enforceFake{revision: "rev-9", afterGets: 1 << 30}
		err := waitACLEnforced(context.Background(), f, 1, "rev-9")
		if err == nil {
			t.Error("want a timeout error so the caller can report enforcement is still pending")
		}
	})

	t.Run("no revision to track is not an error", func(t *testing.T) {
		f := &enforceFake{revision: "", afterGets: 1}
		if err := waitACLEnforced(context.Background(), f, 1, ""); err != nil {
			t.Errorf("an empty revision (older API / no-change path) must be a no-op: %v", err)
		}
	})
}

// --dry-run is a ROOT persistent flag ("print commands; change nothing") that
// this command accepted and then ignored: it issued a live PUT and rewrote a
// cluster's control-plane ACL. Anyone dry-running first as a safety step got the
// mutation they were checking for. Verified against a live cluster before the
// fix — the ACL revision-id changed.
func TestRunnerACLDryRunMakesNoWrites(t *testing.T) {
	d := testDeps(t)
	for _, mode := range []string{"open", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}}}
			withFakeACL(t, fake)
			// For revoke, the IP must be PRESENT so the non-dry path would have
			// something to remove — otherwise the test passes for the wrong reason.
			if mode == "revoke" {
				fake.acl.IPv4 = append(fake.acl.IPv4, "1.2.3.4")
			}
			o := ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4", FailOnMissing: true, DryRun: true}
			if err := RunACL(d, mode, o); err != nil {
				t.Fatalf("%s --dry-run = %v", mode, err)
			}
			if len(fake.puts) != 0 {
				t.Errorf("%s --dry-run issued %d PUT(s): %+v", mode, len(fake.puts), fake.puts)
			}
			// The state file is a write too, and a bogus one: recording
			// Modified=true when nothing changed would make the paired revoke try
			// to remove an IP this run never added.
			if _, ok, _ := readRunnerACLState("e2e"); ok {
				t.Errorf("%s --dry-run wrote a runner-acl state file", mode)
			}
		})
	}
}

// The dry-run branch must still do the real RESOLUTION work — a dry run that
// silently skips cluster lookup would hide the failure it exists to surface.
func TestRunnerACLDryRunStillResolvesAndReads(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}}}
	withFakeACL(t, fake)
	o := ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4", FailOnMissing: true, DryRun: true}
	if err := RunACL(d, "open", o); err != nil {
		t.Fatalf("open --dry-run = %v", err)
	}
	if fake.gets == 0 {
		t.Error("dry-run never read the ACL — it cannot report what would change")
	}
}
