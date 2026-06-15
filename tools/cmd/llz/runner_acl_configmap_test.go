package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
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
		if f.patches <= f.patchNF {
			return `Error from server (NotFound): configmaps "firewall-runner-acl" not found`, errString("exit 1")
		}
		return "configmap/firewall-runner-acl patched", nil
	case "apply":
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

	registerRunnerACLIP("1.2.3.4")

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

	registerRunnerACLIP("1.2.3.4")

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

	registerRunnerACLIP("1.2.3.4")

	var applied bool
	for _, c := range fake.calls {
		if c.args[0] == "apply" {
			applied = true
			if !strings.Contains(c.stdin, "kind: ConfigMap") || !strings.Contains(c.stdin, "ip-1.2.3.4") {
				t.Errorf("apply manifest missing fields:\n%s", c.stdin)
			}
		}
	}
	if !applied {
		t.Errorf("expected apply to create the ConfigMap on NotFound")
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
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}}}
	withFakeACL(t, fake)
	k := &fakeACLKubectl{getJSON: `{"data":{}}`}
	withFakeKubectl(t, k, time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC))

	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4", failOnMissing: true, configMap: true}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if data := k.lastPatchData(t); data["ip-1.2.3.4"] == nil {
		t.Errorf("expected a lease patch for 1.2.3.4, got %v", data)
	}
}

// revoke with --runner-configmap releases the lease even when open made no ACL
// change (Modified=false), and does so before any error path.
func TestRunnerACLRevokeReleasesLease(t *testing.T) {
	fake := &fakeACLClient{acl: linode.ControlPlaneACL{Enabled: true, IPv4: []string{"1.2.3.4/32"}}}
	withFakeACL(t, fake)
	k := &fakeACLKubectl{getJSON: `{"data":{}}`}
	withFakeKubectl(t, k, time.Date(2026, 6, 12, 17, 0, 0, 0, time.UTC))

	// open (no ACL change — already present) records state + leases.
	if err := runRunnerACL("open", runnerACLOpts{region: "e2e", clusterID: "5", ip: "1.2.3.4", configMap: true}); err != nil {
		t.Fatalf("open = %v", err)
	}
	if err := runRunnerACL("revoke", runnerACLOpts{region: "e2e", configMap: true}); err != nil {
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
