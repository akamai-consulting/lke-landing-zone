package clusteraccess

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// kubectlCall is one recorded invocation of the seamed kubectl runner.
type kubectlCall struct {
	args  []string
	stdin string
}

// fakeACLKubectl records calls and serves a scripted reply per (verb,resource).
type fakeACLKubectl struct {
	calls   []kubectlCall
	getJSON string // reply for `get configmap ... -o json`; "" => NotFound
	getErr  bool   // get returns a transport-style error
	patchNF int    // first N `patch` calls fail NotFound, then succeed
	patches int
	// createAlreadyExists makes `create` lose the race, which is the answer the
	// old `apply` fallback could never produce.
	createAlreadyExists bool
	// exists tracks whether the ConfigMap is now present — set by any create,
	// including one that lost the race. See the patch case.
	exists bool
}

func (f *fakeACLKubectl) run(stdin string, args ...string) (string, error) {
	f.calls = append(f.calls, kubectlCall{args: args, stdin: stdin})
	switch args[0] {
	case "get":
		if f.getErr {
			return "", errString("connection refused")
		}
		if f.getJSON == "" {
			return `Error from server (NotFound): configmaps "firewall-runner-acl" not found`, errString("exit 1")
		}
		return f.getJSON, nil
	case "patch":
		f.patches++
		// Once ANYTHING has created the ConfigMap — us, or the racer whose
		// AlreadyExists we just got — a patch can no longer be NotFound. Modelling
		// that is what makes a lost-race test able to discriminate: without it the
		// patch keeps returning NotFound forever and both the old fall-through and
		// the immediate retry fail identically.
		if f.patches <= f.patchNF && !f.exists {
			return `Error from server (NotFound): configmaps "firewall-runner-acl" not found`, errString("exit 1")
		}
		return "configmap/firewall-runner-acl patched", nil
	case "apply":
		return "configmap/firewall-runner-acl created", nil
	case "create":
		if f.createAlreadyExists {
			f.exists = true // the racer's ConfigMap is now there
			return `Error from server (AlreadyExists): configmaps "firewall-runner-acl" already exists`, errString("exit 1")
		}
		f.exists = true
		return "configmap/firewall-runner-acl created", nil
	}
	return "", nil
}

type errString string

func (e errString) Error() string { return string(e) }

// withFakeKubectl points the ConfigMap seams at fake and freezes time.
func withFakeKubectl(t *testing.T, fake *fakeACLKubectl, now time.Time) {
	t.Helper()
	prevK, prevNow, prevSleep := runnerACLKubectlFn, runnerACLNow, runnerACLSleep
	runnerACLKubectlFn = fake.run
	runnerACLNow = func() time.Time { return now }
	runnerACLSleep = func(time.Duration) {}
	t.Cleanup(func() {
		runnerACLKubectlFn, runnerACLNow, runnerACLSleep = prevK, prevNow, prevSleep
	})
}

func (f *fakeACLKubectl) lastPatchData(t *testing.T) map[string]any {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		c := f.calls[i]
		if c.args[0] != "patch" {
			continue
		}
		// args: patch configmap NAME -n NS --type merge -p <body>
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(c.args[len(c.args)-1]), &body); err != nil {
			t.Fatalf("patch body not JSON: %v", err)
		}
		return body.Data
	}
	t.Fatalf("no patch call recorded")
	return nil
}

func TestRunnerACLDataKeySanitizes(t *testing.T) {
	if got := runnerACLDataKey("1.2.3.4"); got != "ip-1.2.3.4" {
		t.Errorf("dataKey(1.2.3.4) = %q", got)
	}
	if got := runnerACLDataKey("1.2.3.4/32"); got != "ip-1.2.3.4-32" {
		t.Errorf("dataKey(/32) = %q, want slash sanitized", got)
	}
}

func TestRegisterLeasesIPWithTTL(t *testing.T) {
	now := time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC)
	fake := &fakeACLKubectl{getJSON: `{"data":{}}`}
	withFakeKubectl(t, fake, now)

	_ = registerRunnerACLIP("1.2.3.4", nil)

	data := fake.lastPatchData(t)
	raw, ok := data["ip-1.2.3.4"].(string)
	if !ok {
		t.Fatalf("lease key missing; data=%v", data)
	}
	var lv runnerACLLeaseValue
	if err := json.Unmarshal([]byte(raw), &lv); err != nil {
		t.Fatalf("lease value not JSON: %v", err)
	}
	if lv.CIDR != "1.2.3.4" {
		t.Errorf("cidr = %q", lv.CIDR)
	}
	want := now.Add(runnerACLLeaseTTL).UTC().Format(time.RFC3339)
	if lv.ExpiresAt != want {
		t.Errorf("expiresAt = %q, want %q", lv.ExpiresAt, want)
	}
}

func TestRegisterPrunesExpiredLeasesInSamePatch(t *testing.T) {
	now := time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC)
	stale := runnerACLLeaseValue{CIDR: "9.9.9.9", ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339)}
	fresh := runnerACLLeaseValue{CIDR: "8.8.8.8", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	staleJSON, _ := json.Marshal(stale)
	freshJSON, _ := json.Marshal(fresh)
	fake := &fakeACLKubectl{getJSON: `{"data":{"ip-9.9.9.9":` + jsonString(string(staleJSON)) +
		`,"ip-8.8.8.8":` + jsonString(string(freshJSON)) + `}}`}
	withFakeKubectl(t, fake, now)

	_ = registerRunnerACLIP("1.2.3.4", nil)

	data := fake.lastPatchData(t)
	if _, ok := data["ip-9.9.9.9"]; !ok || data["ip-9.9.9.9"] != nil {
		t.Errorf("expired lease should be nulled, got %v", data["ip-9.9.9.9"])
	}
	if _, ok := data["ip-8.8.8.8"]; ok {
		t.Errorf("fresh lease must NOT be touched, got %v", data["ip-8.8.8.8"])
	}
	if data["ip-1.2.3.4"] == nil {
		t.Errorf("own lease missing from patch")
	}
}

func TestRegisterCreatesConfigMapWhenAbsent(t *testing.T) {
	now := time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC)
	fake := &fakeACLKubectl{getJSON: "", patchNF: 99} // get => NotFound, patch always NotFound
	withFakeKubectl(t, fake, now)

	_ = registerRunnerACLIP("1.2.3.4", nil)

	var created bool
	for _, c := range fake.calls {
		if c.args[0] == "apply" {
			t.Error("the NotFound fallback must not `apply`. It carries ONE key — this runner's lease — " +
				"and apply is an upsert whose three-way merge computes DELETIONS from the live object's " +
				"last-applied-configuration. Two runners both seeing NotFound means the second one's apply " +
				"REMOVES the first one's lease, the controller evicts that runner's IP from the " +
				"control-plane ACL, and its kubectl calls start failing mid-job.")
		}
		if c.args[0] == "create" {
			created = true
			if !strings.Contains(c.stdin, "kind: ConfigMap") || !strings.Contains(c.stdin, "ip-1.2.3.4") {
				t.Errorf("create manifest missing fields:\n%s", c.stdin)
			}
		}
	}
	if !created {
		t.Errorf("expected create to make the ConfigMap on NotFound")
	}
}

// TestRegisterRetriesThePatchWhenItRacesACreator. `create` returning AlreadyExists
// is the answer the isAlreadyExists branch was written for — and could never
// receive, because `apply` does not return it. That branch was dead code guarding
// against the exact race the call it guarded was causing.
func TestRegisterRetriesThePatchWhenItRacesACreator(t *testing.T) {
	now := time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC)
	// NotFound on the first patch, then a creator wins the race, then our retry
	// patch succeeds against the ConfigMap they made.
	fake := &fakeACLKubectl{getJSON: "", patchNF: 1, createAlreadyExists: true}
	withFakeKubectl(t, fake, now)

	if err := registerRunnerACLIP("1.2.3.4", nil); err != nil {
		t.Fatalf("racing a creator must converge, not fail: %v", err)
	}
	var patchesAfterCreate int
	seenCreate := false
	for _, c := range fake.calls {
		switch {
		case c.args[0] == "create":
			seenCreate = true
		case c.args[0] == "patch" && seenCreate:
			patchesAfterCreate++
		}
	}
	if !seenCreate {
		t.Fatal("no create attempt — this test is not exercising the race")
	}
	if patchesAfterCreate == 0 {
		t.Error("after losing the create race the lease must be re-PATCHED onto the winner's ConfigMap; " +
			"giving up here leaves this runner with no lease and the controller evicts its IP")
	}
}

// TestRegisterRecoversFromALostRaceOnTheFINALAttempt.
//
// Falling through to the next lap spent the attempt on a ConfigMap that
// demonstrably EXISTS — and on the LAST attempt it exited with no lease at all,
// where the old `apply` would at least have written one. A backstop must not cost
// the thing it protects.
func TestRegisterRecoversFromALostRaceOnTheFINALAttempt(t *testing.T) {
	now := time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC)
	// ONE attempt, so there is no next lap to fall through to — which is the only
	// configuration that tells the two behaviours apart. With more attempts the
	// fall-through simply succeeds on the following lap and the test passes either
	// way, which is what a first cut of it did.
	prevN := runnerACLPatchN
	runnerACLPatchN = 1
	t.Cleanup(func() { runnerACLPatchN = prevN })

	// The patch is NotFound until something creates the ConfigMap; the create then
	// loses the race to another runner.
	fake := &fakeACLKubectl{getJSON: "", patchNF: 99, createAlreadyExists: true}
	withFakeKubectl(t, fake, now)

	_ = registerRunnerACLIP("1.2.3.4", nil)

	// ASSERTED ON THE CALLS, not on the error: leaseOutcome always returns nil —
	// the lease is best-effort by design — so an error assertion here can never
	// tell the two behaviours apart. A first cut of this test did exactly that and
	// passed with the fix reverted.
	seenCreate := false
	patchedAfterCreate := false
	for _, c := range fake.calls {
		switch {
		case c.args[0] == "create":
			seenCreate = true
		case c.args[0] == "patch" && seenCreate:
			patchedAfterCreate = true
		}
	}
	if !seenCreate {
		t.Fatal("no create attempt — this test is not exercising the race")
	}
	if !patchedAfterCreate {
		t.Error("losing the race on the LAST attempt left no lease: falling through spends the attempt " +
			"on a ConfigMap that demonstrably EXISTS, and there is no next lap to patch it on — where " +
			"the old `apply` would at least have written one")
	}
}

func TestDeregisterNullsLeaseKey(t *testing.T) {
	now := time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC)
	fake := &fakeACLKubectl{getJSON: `{"data":{}}`}
	withFakeKubectl(t, fake, now)

	deregisterRunnerACLIP("1.2.3.4")

	data := fake.lastPatchData(t)
	if v, ok := data["ip-1.2.3.4"]; !ok || v != nil {
		t.Errorf("deregister should null the lease key, got data=%v", data)
	}
}

// open with --runner-configmap leases the IP after the ACL PUT.
func TestRunnerACLOpenLeasesWhenConfigMapEnabled(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}}}
	withFakeACL(t, fake)
	k := &fakeACLKubectl{getJSON: `{"data":{}}`}
	withFakeKubectl(t, k, time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC))

	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4", FailOnMissing: true, ConfigMap: true}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if data := k.lastPatchData(t); data["ip-1.2.3.4"] == nil {
		t.Errorf("expected a lease patch for 1.2.3.4, got %v", data)
	}
}

// revoke with --runner-configmap releases the lease even when open made no ACL
// change (Modified=false), and does so before any error path.
func TestRunnerACLRevokeReleasesLease(t *testing.T) {
	d := testDeps(t)
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}}}
	withFakeACL(t, fake)
	k := &fakeACLKubectl{getJSON: `{"data":{}}`}
	withFakeKubectl(t, k, time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC))

	// open (no ACL change — already present) records state + leases.
	if err := RunACL(d, "open", ACLOpts{Region: "e2e", ClusterID: "5", Ip: "1.2.3.4", ConfigMap: true}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if err := RunACL(d, "revoke", ACLOpts{Region: "e2e", ConfigMap: true}); err != nil {
		t.Fatalf("revoke = %v", err)
	}
	if data := k.lastPatchData(t); data["ip-1.2.3.4"] != nil {
		t.Errorf("revoke should null the lease key, got %v", data["ip-1.2.3.4"])
	}
}

// jsonString quotes s as a JSON string literal for embedding in a JSON document.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The lease is registered against an apiserver that may not yet accept this
// runner — that is what the retry loop is for. Without a per-call bound, kubectl
// sits in connect/TLS until the kernel gives up (~2m), so runnerACLPatchN=8 means
// ~15 minutes, not 8 quick tries. A live e2e run burned 15m25s here and still
// reported success, because a lease failure is deliberately only a warning.
//
// This asserts the real argv, not the test seam: the fake replaces
// runnerACLKubectlFn wholesale, so a flag added inside that closure would be
// invisible to every other test in this file.
func TestRunnerACLKubectlArgvIsBounded(t *testing.T) {
	got := runnerACLKubectlArgv([]string{"apply", "-f", "-"})
	if len(got) == 0 || !strings.HasPrefix(got[0], "--request-timeout=") {
		t.Fatalf("kubectl argv must START with --request-timeout: %v\n"+
			"Appended, it lands after `-f -` and kubectl reads it as a positional argument.", got)
	}
	if got[0] == "--request-timeout=0" || got[0] == "--request-timeout=0s" {
		t.Errorf("--request-timeout=0 means NO timeout in kubectl — the opposite of the intent: %v", got)
	}
	if want := []string{"apply", "-f", "-"}; !reflect.DeepEqual(got[1:], want) {
		t.Errorf("original argv must survive unchanged: got %v, want %v", got[1:], want)
	}
	// The point of the bound is that the whole retry loop is finite and short
	// enough that the step timeout above it is the backstop, not the mechanism.
	if budget := time.Duration(runnerACLPatchN) * (runnerACLKubectlTimeout + runnerACLPatchGap); budget > 5*time.Minute {
		t.Errorf("worst-case lease budget is %s — too close to the step timeout to be a useful bound", budget)
	}
}

// THE DEADLOCK. The caller adds the runner IP to the control-plane ACL and
// verifies it, then leases it here so the firewall controller preserves it. If a
// controller reconcile lands in between, the runner is evicted — and the lease
// can never be written, because writing it needs the apiserver the runner was
// just locked out of. Every remaining retry then fails, since the loop retries
// kubectl rather than the thing that actually broke.
//
// Seen twice on live e2e runs (30473785350, 30478772336): "added <ip> to cluster
// <id> control-plane ACL" followed by kubectl hanging until the step budget
// expired, and the apiserver never becoming reachable again.
//
// This models exactly that: kubectl fails while evicted, and only re-asserting
// the ACL can unstick it. Without the reassert wiring the lease never lands, so
// this test fails — which is the point.
func TestRegisterRunnerACLIPRecoversFromEviction(t *testing.T) {
	evicted := true // a reconcile clobbered us between the ACL add and now
	patches := 0

	prevK, prevSleep := runnerACLKubectlFn, runnerACLSleep
	runnerACLKubectlFn = func(_ string, args ...string) (string, error) {
		if args[0] == "patch" {
			patches++
			if evicted {
				// What an evicted runner actually gets back.
				return "", errString("dial tcp 1.2.3.4:443: i/o timeout")
			}
			return "configmap/firewall-runner-acl patched", nil
		}
		return "", nil
	}
	runnerACLSleep = func(time.Duration) {}
	t.Cleanup(func() { runnerACLKubectlFn, runnerACLSleep = prevK, prevSleep })

	reasserts := 0
	registerRunnerACLIP("1.2.3.4", func() error {
		reasserts++
		evicted = false // putting the IP back restores apiserver reachability
		return nil
	})

	if reasserts == 0 {
		t.Fatal("the ACL was never re-asserted — a failed lease is the ONLY signal that a reconcile " +
			"evicted this runner, so retrying kubectl alone can never succeed and the job deadlocks")
	}
	if patches < 2 {
		t.Errorf("expected a retry after the re-assert, got %d patch attempt(s)", patches)
	}
	if evicted {
		t.Error("still evicted after the loop — the lease never landed")
	}
}

// A re-assert that itself fails must not abort the loop or panic: the ACL add is
// best-effort by design (the direct grant already happened), and the lease is
// warn-only. Losing the retry to an unrelated Linode API blip would reintroduce
// the deadlock through a different door.
func TestRegisterRunnerACLIPSurvivesFailingReassert(t *testing.T) {
	prevK, prevSleep := runnerACLKubectlFn, runnerACLSleep
	runnerACLKubectlFn = func(_ string, args ...string) (string, error) {
		return "", errString("dial tcp: i/o timeout")
	}
	runnerACLSleep = func(time.Duration) {}
	t.Cleanup(func() { runnerACLKubectlFn, runnerACLSleep = prevK, prevSleep })

	calls := 0
	registerRunnerACLIP("1.2.3.4", func() error { calls++; return errString("linode 500") })

	if calls != runnerACLPatchN-1 {
		t.Errorf("re-assert should be attempted between every retry: got %d, want %d", calls, runnerACLPatchN-1)
	}
}

// The lease is BEST-EFFORT — the ACL grant already happened — so it must never
// be able to consume the caller's step budget. It did: an e2e run
// (30482110220) sat here for 5m48s and emitted NOTHING, because the only warning
// came after all 8 attempts and the 6m step timeout killed it first. The step
// then failed with "The action has timed out" and no clue which call was stuck.
//
// A phase that cannot report why it gave up is worse than one that gives up
// early, so this pins the ceiling: slow attempts must trip the budget and
// RETURN, not run the loop to completion.
func TestRegisterRunnerACLIPHonoursLeaseBudget(t *testing.T) {
	clock := time.Now()
	prevK, prevNow, prevSleep := runnerACLKubectlFn, runnerACLNow, runnerACLSleep
	// Every kubectl burns 45s of wall-clock, so attempt 3 is past the 90s budget.
	runnerACLKubectlFn = func(_ string, args ...string) (string, error) {
		if args[0] == "patch" {
			clock = clock.Add(45 * time.Second)
			return "", errString("dial tcp: i/o timeout")
		}
		return "", nil
	}
	runnerACLNow = func() time.Time { return clock }
	runnerACLSleep = func(time.Duration) {}
	t.Cleanup(func() { runnerACLKubectlFn, runnerACLNow, runnerACLSleep = prevK, prevNow, prevSleep })

	attempts := 0
	done := make(chan struct{})
	go func() {
		registerRunnerACLIP("1.2.3.4", func() error { attempts++; return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("registerRunnerACLIP never returned — the lease phase must be bounded")
	}

	// 45s per attempt against a 90s budget must stop after ~2, NOT run all 8.
	// Asserting merely "< runnerACLPatchN" is useless: the loop only re-asserts
	// between attempts, so it reaches 7 even with no budget at all — a bound that
	// tests nothing. The number has to be tight enough to distinguish them.
	if want := 3; attempts > want {
		t.Errorf("ran %d attempts at 45s each; the %s budget should have stopped the loop by ~%d "+
			"(unbounded, it reaches %d and blows the step timeout)",
			attempts, runnerACLLeaseBudget, want, runnerACLPatchN-1)
	}
	if runnerACLLeaseBudget >= 6*time.Minute {
		t.Errorf("lease budget %s is not comfortably under the 6m step timeout that killed the run",
			runnerACLLeaseBudget)
	}
}

// A lease failure is best-effort ONLY while its premise holds — that the
// Linode-API ACL grant above it already granted access. When kubectl cannot reach
// the apiserver at all, that premise is disproved and continuing wastes the job:
// run 30499831638 reported "Cluster access: success" while holding proof that
// every request timed out, then burned 900s in a generic "waiting for the control
// plane" loop before failing with no cause.
//
// Strings are the REAL ones from that run's log, not paraphrases.
func TestIsAPIServerUnreachable(t *testing.T) {
	unreachable := []string{
		`couldn't get current server API group list: Get "https://lke637329.api.us-ord.enterprise.linodelke.net:6443/api?timeout=10s": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)`,
		`client rate limiter Wait returned an error: context deadline exceeded - error from a previous attempt: EOF`,
		`Unable to connect to the server: dial tcp 1.2.3.4:6443: connect: connection refused`,
		`dial tcp 1.2.3.4:6443: i/o timeout`,
		`net/http: TLS handshake timeout`,
	}
	for _, s := range unreachable {
		if !isAPIServerUnreachable(s) {
			t.Errorf("must be treated as UNREACHABLE (hard fail), got warn-only:\n  %s", s)
		}
	}

	// Reached the apiserver: it answered. These must stay warnings — access works,
	// so only the lease is missing and the job can legitimately continue.
	reachable := []string{
		`Error from server (NotFound): configmaps "firewall-runner-acl" not found`,
		`Error from server (Forbidden): configmaps "firewall-runner-acl" is forbidden: User cannot patch resource`,
		`Error from server (AlreadyExists): configmaps "firewall-runner-acl" already exists`,
		`configmap/firewall-runner-acl patched`,
		``,
	}
	for _, s := range reachable {
		if isAPIServerUnreachable(s) {
			t.Errorf("apiserver ANSWERED, so this must stay a warning — hard-failing here would break\n"+
				"legitimate best-effort behaviour (RBAC/NotFound races):\n  %s", s)
		}
	}
}

// leaseOutcome must NEVER fail the step — including when the apiserver is
// unreachable.
//
// An earlier version failed on exactly that, reasoning it disproved the "the grant
// already worked" premise. Measurement killed that: on a freshly created LKE-E
// cluster the control-plane ACL takes MINUTES to honour a newly added address
// (vs ~35s on a settled one, measured against the Cloud Manager). So "unreachable"
// is the expected state during a cold bootstrap, and failing on it aborts the run
// at cluster-access — before `wait-cluster-ready`, the step whose entire purpose is
// to wait that out, has even started.
//
// Reachability is that step's verdict to make. This one only reports.
func TestLeaseOutcomeNeverFailsTheStep(t *testing.T) {
	unreachableOut := `couldn't get current server API group list: ... EOF`
	if err := leaseOutcome("1.2.3.4", 1, 110*time.Second, errString("exit status 1"), unreachableOut); err != nil {
		t.Errorf("an unreachable apiserver is NORMAL on a cold bootstrap and must not fail the step "+
			"(wait-cluster-ready owns reachability), got: %v", err)
	}
	forbidden := `Error from server (Forbidden): configmaps is forbidden`
	if err := leaseOutcome("1.2.3.4", 8, 90*time.Second, errString("exit status 1"), forbidden); err != nil {
		t.Errorf("a reachable-but-refused apiserver must stay best-effort, got: %v", err)
	}
	// The classification still has to WORK — it drives which message is printed,
	// and conflating the two would mislead whoever reads the log next.
	if !isAPIServerUnreachable(unreachableOut) || isAPIServerUnreachable(forbidden) {
		t.Error("isAPIServerUnreachable must still distinguish never-reached from answered-and-refused")
	}
}
