package health

import (
	"encoding/json"
	"testing"
)

func TestParseBaoStatus(t *testing.T) {
	// Active leader: initialized true, sealed false, is_self true.
	active, err := ParseBaoStatus([]byte(`{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !active.Initialized || active.Sealed || active.HAMode != "active" {
		t.Errorf("active leader parsed wrong: %+v", active)
	}
	// Standby: ha_enabled but not is_self.
	standby, _ := ParseBaoStatus([]byte(`{"initialized":true,"sealed":false,"is_self":false,"ha_enabled":true}`))
	if standby.HAMode != "standby" {
		t.Errorf("standby HA mode = %q", standby.HAMode)
	}
	// The null-handling fix: a sealed pod that omits fields must default
	// initialized=false, sealed=true (fail-safe), NOT be clobbered to unsealed.
	empty, _ := ParseBaoStatus([]byte(`{}`))
	if empty.Initialized || !empty.Sealed || empty.HAMode != "standalone" {
		t.Errorf("empty status should be uninitialized+sealed+standalone, got %+v", empty)
	}
	// A literal false for sealed must be preserved (the `//` bug this guards).
	unsealed, _ := ParseBaoStatus([]byte(`{"initialized":true,"sealed":false}`))
	if unsealed.Sealed {
		t.Error("literal sealed=false must be preserved, not defaulted to true")
	}
}

func TestClassifyBaoSeal(t *testing.T) {
	if cat, _ := ClassifyBaoSeal(BaoStatus{Initialized: true, Sealed: false, HAMode: "active"}); cat != CatOK {
		t.Error("initialized+unsealed should pass")
	}
	if cat, _ := ClassifyBaoSeal(BaoStatus{Initialized: false}); cat != CatFail {
		t.Error("uninitialized should fail")
	}
	if cat, _ := ClassifyBaoSeal(BaoStatus{Initialized: true, Sealed: true}); cat != CatFail {
		t.Error("sealed should fail")
	}
}

func TestClassifyLeaderCount(t *testing.T) {
	if cat, _ := ClassifyLeaderCount(3, 1); cat != CatOK {
		t.Error("exactly one leader is healthy")
	}
	if cat, _ := ClassifyLeaderCount(3, 0); cat != CatFail {
		t.Error("no leader should fail")
	}
	if cat, _ := ClassifyLeaderCount(3, 2); cat != CatFail {
		t.Error("split-brain should fail")
	}
	if cat, _ := ClassifyLeaderCount(0, 0); cat != CatOK {
		t.Error("no replicas (skip) is not a leader failure")
	}
}

func TestCountReadyEndpointsAndWebhook(t *testing.T) {
	const raw = `[
      {"endpoints": [{"conditions": {"ready": true}}, {"conditions": {"ready": false}}]},
      {"endpoints": [{"conditions": {}}]}
    ]`
	var slices []EndpointSlice
	if err := json.Unmarshal([]byte(raw), &slices); err != nil {
		t.Fatal(err)
	}
	// 1 explicit-true + 1 absent(=>true) = 2; the explicit-false is excluded.
	if got := CountReadyEndpoints(slices); got != 2 {
		t.Errorf("CountReadyEndpoints = %d, want 2", got)
	}
	if cat, _ := ClassifyWebhookBackend(true, 2); cat != CatOK {
		t.Error("webhook with ready endpoints ok")
	}
	if cat, _ := ClassifyWebhookBackend(true, 0); cat != CatFail {
		t.Error("webhook with 0 endpoints fails")
	}
	if cat, _ := ClassifyWebhookBackend(false, 0); cat != CatFail {
		t.Error("missing backing service fails")
	}
}

func TestFirewallClassifiers(t *testing.T) {
	if cat, _ := ClassifyFirewallToken(true, "abc"); cat != CatOK {
		t.Error("non-empty token ok")
	}
	if cat, _ := ClassifyFirewallToken(true, ""); cat != CatFail {
		t.Error("empty token fails")
	}
	if cat, _ := ClassifyFirewallToken(false, ""); cat != CatFail {
		t.Error("missing secret fails")
	}
	if ClassifyFirewallConfigKey("LINODE_FIREWALL_ID", "123") != CatOK {
		t.Error("set key ok")
	}
	if ClassifyFirewallConfigKey("LINODE_FIREWALL_ID", "") != CatDeferred {
		t.Error("empty firewall id is deferred")
	}
	if ClassifyFirewallConfigKey("LKE_CLUSTER_ID", "") != CatOK {
		t.Error("empty cluster id is informational (not deferred)")
	}
	if ClassifyFirewallConfigKey("FIREWALL_TEMPLATE_ID", "") != CatDeferred {
		t.Error("empty manifest-owned key is deferred")
	}
}

func TestClassifyControlPlaneACL(t *testing.T) {
	enabled := func(ipv4, ipv6 []string) ControlPlaneACLState {
		return ControlPlaneACLState{Enabled: true, IPv4: ipv4, IPv6: ipv6}
	}

	tests := []struct {
		name      string
		acl       ControlPlaneACLState
		wantIPv4  []string
		wantIPv6  []string
		expectCat Category
	}{
		{
			name:      "disabled fails even when contents would match",
			acl:       ControlPlaneACLState{Enabled: false, IPv4: []string{"1.2.3.4/32"}},
			wantIPv4:  []string{"1.2.3.4/32"},
			expectCat: CatFail,
		},
		{
			name:      "nothing expected is informational",
			acl:       enabled([]string{"9.9.9.9/32"}, nil),
			expectCat: CatWarn,
		},
		{
			name:      "all expected present passes",
			acl:       enabled([]string{"1.2.3.4/32", "5.6.7.8/32", "20.0.0.1/32"}, []string{"2600::1/128"}),
			wantIPv4:  []string{"1.2.3.4/32", "5.6.7.8/32"},
			wantIPv6:  []string{"2600::1/128"},
			expectCat: CatOK,
		},
		{
			name:      "missing an expected IPv4 fails",
			acl:       enabled([]string{"1.2.3.4/32"}, nil),
			wantIPv4:  []string{"1.2.3.4/32", "5.6.7.8/32"},
			expectCat: CatFail,
		},
		{
			name:      "missing an expected IPv6 fails",
			acl:       enabled([]string{"1.2.3.4/32"}, nil),
			wantIPv4:  []string{"1.2.3.4/32"},
			wantIPv6:  []string{"2600::1/128"},
			expectCat: CatFail,
		},
		{
			// Linode normalizes bare host IPs to /32 (and /128); a bare expected IP
			// satisfied by a /32 live entry (and vice versa) is NOT a miss.
			name:      "bare host vs single-host CIDR are equal",
			acl:       enabled([]string{"1.2.3.4/32", "5.6.7.8"}, []string{"2600::1"}),
			wantIPv4:  []string{"1.2.3.4", "5.6.7.8/32"},
			wantIPv6:  []string{"2600::1/128"},
			expectCat: CatOK,
		},
		{
			// Extra live entries (CI-runner leases, operator additions) never fail
			// the superset check.
			name:      "extra live entries are fine",
			acl:       enabled([]string{"1.2.3.4/32", "203.0.113.9/32"}, nil),
			wantIPv4:  []string{"1.2.3.4/32"},
			expectCat: CatOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if cat, msg := ClassifyControlPlaneACL(tt.acl, tt.wantIPv4, tt.wantIPv6); cat != tt.expectCat {
				t.Errorf("got %v (%q), want %v", cat, msg, tt.expectCat)
			}
		})
	}
}
