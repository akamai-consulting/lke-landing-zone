package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultStorageClasses(t *testing.T) {
	raw := []byte(`{"items":[
	  {"metadata":{"name":"linode-block-storage","annotations":{"storageclass.kubernetes.io/is-default-class":"false"}}},
	  {"metadata":{"name":"block-storage-retain","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}},
	  {"metadata":{"name":"plain"}}
	]}`)
	got, err := defaultStorageClasses(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "block-storage-retain" {
		t.Fatalf("expected only the class annotated \"true\", got %v", got)
	}
	// The annotation is a string; reading the KEY's presence rather than its value
	// would report the "false" class as default too.
	if _, err := defaultStorageClasses([]byte(`not json`)); err == nil {
		t.Error("an unparseable list must be an error, not an empty set")
	}
}

func TestEvalSCDemote(t *testing.T) {
	if v := evalSCDemote([]string{"block-storage-retain"}); v.FailWhy != "" {
		t.Errorf("exactly one default must pass: %s", v.FailWhy)
	}
	two := evalSCDemote([]string{"a", "b"})
	if two.FailWhy == "" {
		t.Error("two defaults must fail — this is the PR #101 convergence wedge")
	}
	if !strings.Contains(two.FailWhy, "a") || !strings.Contains(two.FailWhy, "b") {
		t.Errorf("the failure must name the competing classes, got %q", two.FailWhy)
	}
	// Zero is a failure too: a demoter that demoted the last default breaks every
	// PVC that omits storageClassName, and "0" is a plausible bug for THIS lane.
	if v := evalSCDemote(nil); v.FailWhy == "" {
		t.Error("zero defaults must fail — it is the demote lane's own worst case")
	}
}

func TestEvalTokenInventory(t *testing.T) {
	now := time.Unix(1_720_000_000, 0).UTC()
	fresh := []byte(`{"data":{"updated":"` + now.Add(-10*time.Minute).Format(time.RFC3339) + `"}}`)
	if v := evalTokenInventory(fresh, now, 3*time.Hour); v.FailWhy != "" {
		t.Errorf("a 10m-old inventory must pass: %s", v.FailWhy)
	}
	stale := []byte(`{"data":{"updated":"` + now.Add(-9*time.Hour).Format(time.RFC3339) + `"}}`)
	if v := evalTokenInventory(stale, now, 3*time.Hour); v.FailWhy == "" {
		t.Error("a 9h-old inventory must fail — the writer has stopped")
	}
	// No timestamp is a FAILURE, not a skip: an inventory whose age cannot be
	// established is exactly as blind as an old one, and the downstream gauges
	// keep serving last-known-good either way.
	if v := evalTokenInventory([]byte(`{"data":{}}`), now, time.Hour); v.FailWhy == "" {
		t.Error("a ConfigMap with no update timestamp must fail closed")
	}
	if v := evalTokenInventory([]byte(`{"data":{"updated":"last tuesday"}}`), now, time.Hour); v.FailWhy == "" {
		t.Error("an unparseable timestamp must fail closed")
	}
	if v := evalTokenInventory([]byte(`nope`), now, time.Hour); v.FailWhy == "" {
		t.Error("an unparseable ConfigMap must fail closed")
	}
}

func TestEvalCIDRFirewall(t *testing.T) {
	ok := []byte(`{"data":{"LKE_CLUSTER_ID":"637937","VPC_CIDR":"10.0.0.0/16"}}`)
	if v := evalCIDRFirewall(ok); v.FailWhy != "" {
		t.Errorf("a populated ConfigMap must pass: %s", v.FailWhy)
	}
	// A blank value is the dangerous case: the key exists, so a presence check
	// passes, and the controller silently runs on its compiled default.
	blank := []byte(`{"data":{"LKE_CLUSTER_ID":"637937","VPC_CIDR":"   "}}`)
	v := evalCIDRFirewall(blank)
	if v.FailWhy == "" {
		t.Error("a blank required value must fail — presence is not the invariant")
	}
	if !strings.Contains(v.FailWhy, "VPC_CIDR") {
		t.Errorf("the failure must name the blank key, got %q", v.FailWhy)
	}
	if v := evalCIDRFirewall([]byte(`{"data":{}}`)); v.FailWhy == "" {
		t.Error("an empty ConfigMap must fail")
	}
}

// seamEffectReaders points every kubectl read at canned bodies/errors.
func seamEffectReaders(t *testing.T, sc, token, fw []byte, scErr, tokenErr, fwErr error) {
	oSC, oTok, oFW := readStorageClasses, readTokenInventory, readFirewallConfig
	t.Cleanup(func() { readStorageClasses, readTokenInventory, readFirewallConfig = oSC, oTok, oFW })
	readStorageClasses = func() ([]byte, error) { return sc, scErr }
	readTokenInventory = func(string) ([]byte, error) { return token, tokenErr }
	readFirewallConfig = func() ([]byte, error) { return fw, fwErr }
}

func TestProbeReconcilerEffectsSkipsDisabledLanes(t *testing.T) {
	seamEffectReaders(t, nil, nil, nil,
		errors.New("must not be called"), errors.New("must not be called"), errors.New("must not be called"))
	// No lanes enabled — every check must SKIP, and a skip must not be a failure.
	vs := probeReconcilerEffects("llz-reconciler", map[string]bool{}, time.Now(), time.Hour)
	if len(failedEffects(vs)) != 0 {
		t.Errorf("disabled lanes must skip, not fail: %+v", failedEffects(vs))
	}
	for _, v := range vs {
		if !v.Skipped {
			t.Errorf("lane %s should be skipped", v.Lane)
		}
	}
}

// An enabled lane whose object cannot be read must FAIL. Degrading to a skip is
// how "we could not tell" becomes a green check on a broken invariant.
func TestProbeReconcilerEffectsFailsWhenEnabledObjectUnreadable(t *testing.T) {
	seamEffectReaders(t, nil, nil, nil,
		errors.New("connection refused"), errors.New("NotFound"), errors.New("NotFound"))
	vs := probeReconcilerEffects("llz-reconciler",
		map[string]bool{"sc-demote": true, "token-inventory": true, "cidr-firewall": true},
		time.Now(), time.Hour)
	if got := len(failedEffects(vs)); got != 3 {
		t.Errorf("all three enabled-but-unreadable invariants must fail, got %d: %+v", got, vs)
	}
}

func TestRunAssertReconcilerEffectsHappyPath(t *testing.T) {
	now := time.Now().UTC()
	seamEffectReaders(t,
		[]byte(`{"items":[{"metadata":{"name":"block-storage-retain","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}`),
		[]byte(`{"data":{"updated":"`+now.Format(time.RFC3339)+`"}}`),
		[]byte(`{"data":{"LKE_CLUSTER_ID":"1","VPC_CIDR":"10.0.0.0/16"}}`),
		nil, nil, nil)
	err := runCIAssertReconcilerEffects("llz-reconciler",
		[]string{"sc-demote", "token-inventory", "cidr-firewall"}, 3*time.Hour, 0, time.Millisecond)
	if err != nil {
		t.Errorf("expected all invariants to hold, got %v", err)
	}
}

func TestRunAssertReconcilerEffectsFailsOnTwoDefaults(t *testing.T) {
	now := time.Now().UTC()
	seamEffectReaders(t,
		[]byte(`{"items":[
		  {"metadata":{"name":"a","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}},
		  {"metadata":{"name":"b","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}
		]}`),
		[]byte(`{"data":{"updated":"`+now.Format(time.RFC3339)+`"}}`),
		[]byte(`{"data":{"LKE_CLUSTER_ID":"1","VPC_CIDR":"10.0.0.0/16"}}`),
		nil, nil, nil)
	err := runCIAssertReconcilerEffects("llz-reconciler",
		[]string{"sc-demote", "token-inventory", "cidr-firewall"}, 3*time.Hour, 0, time.Millisecond)
	if err == nil {
		t.Fatal("two default StorageClasses must fail the gate")
	}
	if !strings.Contains(err.Error(), "sc-demote") {
		t.Errorf("the failure must name the lane, got %v", err)
	}
}

// An unreadable Deployment must fail rather than degrade to "no lanes enabled",
// which would pass having checked nothing.
func TestRunAssertReconcilerEffectsFailsWhenLaneSetUnknown(t *testing.T) {
	orig := enabledReconcilerLanes
	t.Cleanup(func() { enabledReconcilerLanes = orig })
	enabledReconcilerLanes = func(string) ([]string, error) { return nil, errors.New("no deployment") }
	if err := runCIAssertReconcilerEffects("llz-reconciler", nil, time.Hour, 0, time.Millisecond); err == nil {
		t.Error("an unreadable Deployment must fail the gate")
	}
}
