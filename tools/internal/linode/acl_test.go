package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestControlPlaneACLContainsIP(t *testing.T) {
	a := ControlPlaneACL{IPv4: []string{"1.2.3.4/32", "9.9.9.0/24"}}
	if !a.ContainsIP("1.2.3.4") {
		t.Error("ContainsIP should match the /32 form of a bare IP")
	}
	if !a.ContainsIP("9.9.9.0/24") {
		t.Error("ContainsIP should match an exact CIDR entry")
	}
	if a.ContainsIP("5.5.5.5") {
		t.Error("ContainsIP should not match an absent IP")
	}
}

func TestControlPlaneACLWithIP(t *testing.T) {
	a := ControlPlaneACL{Enabled: true, IPv4: []string{"9.9.9.0/24"}, IPv6: []string{"::1/128"}}

	got, changed := a.WithIP("1.1.1.1")
	if !changed {
		t.Fatal("WithIP(new) changed = false, want true")
	}
	if !got.Enabled {
		t.Error("WithIP should enforce the ACL")
	}
	// Sorted + deduped union; IPv6 preserved.
	if !reflect.DeepEqual(got.IPv4, []string{"1.1.1.1", "9.9.9.0/24"}) {
		t.Errorf("WithIP IPv4 = %v, want sorted union", got.IPv4)
	}
	if !reflect.DeepEqual(got.IPv6, []string{"::1/128"}) {
		t.Errorf("WithIP dropped IPv6 = %v", got.IPv6)
	}

	// Already present (bare matches the /32 form) → no change.
	a2 := ControlPlaneACL{Enabled: true, IPv4: []string{"1.1.1.1/32"}}
	if _, changed := a2.WithIP("1.1.1.1"); changed {
		t.Error("WithIP(present-as-/32) changed = true, want false")
	}
}

func TestControlPlaneACLWithoutIP(t *testing.T) {
	a := ControlPlaneACL{Enabled: true, IPv4: []string{"1.1.1.1/32", "9.9.9.0/24"}, IPv6: []string{"::1/128"}}

	got, changed := a.WithoutIP("1.1.1.1") // removes the /32 form
	if !changed {
		t.Fatal("WithoutIP(present) changed = false, want true")
	}
	if !reflect.DeepEqual(got.IPv4, []string{"9.9.9.0/24"}) {
		t.Errorf("WithoutIP IPv4 = %v, want [9.9.9.0/24]", got.IPv4)
	}
	if !got.Enabled || !reflect.DeepEqual(got.IPv6, []string{"::1/128"}) {
		t.Errorf("WithoutIP should preserve Enabled + IPv6, got %+v", got)
	}

	if _, changed := a.WithoutIP("5.5.5.5"); changed {
		t.Error("WithoutIP(absent) changed = true, want false")
	}
}

func TestMatchClusterIDs(t *testing.T) {
	clusters := []map[string]any{
		{"id": json.Number("1"), "label": "lke-e2e", "region": "us-ord"},
		{"id": json.Number("2"), "label": "lke-e2e", "region": "us-sea"},
		{"id": json.Number("3"), "label": "other", "region": "us-ord"},
	}
	// Label only → both us-ord and us-sea match (ambiguous).
	if got := MatchClusterIDs(clusters, "lke-e2e", ""); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Errorf("MatchClusterIDs(label) = %v, want [1 2]", got)
	}
	// Label + region → unique.
	if got := MatchClusterIDs(clusters, "lke-e2e", "us-sea"); !reflect.DeepEqual(got, []uint64{2}) {
		t.Errorf("MatchClusterIDs(label,region) = %v, want [2]", got)
	}
	// No match.
	if got := MatchClusterIDs(clusters, "nope", ""); len(got) != 0 {
		t.Errorf("MatchClusterIDs(absent) = %v, want empty", got)
	}
}

func TestGetControlPlaneACL(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4beta/lke/clusters/7/control_plane_acl" {
			t.Errorf("path = %q, want .../control_plane_acl", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"acl": map[string]any{
				"enabled":   true,
				"addresses": map[string]any{"ipv4": []string{"1.1.1.1/32"}, "ipv6": []string{}},
			},
		})
	})
	acl, err := c.GetControlPlaneACL(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetControlPlaneACL = %v", err)
	}
	if !acl.Enabled || !reflect.DeepEqual(acl.IPv4, []string{"1.1.1.1/32"}) {
		t.Errorf("GetControlPlaneACL = %+v", acl)
	}
}

func TestGetControlPlaneACLEnabledDefaultsTrue(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		// No `enabled` field — must be reported as enforced.
		writeJSON(w, http.StatusOK, map[string]any{
			"acl": map[string]any{"addresses": map[string]any{"ipv4": []string{}}},
		})
	})
	acl, err := c.GetControlPlaneACL(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetControlPlaneACL = %v", err)
	}
	if !acl.Enabled {
		t.Error("absent enabled should be reported as true (enforced)")
	}
}

func TestPutControlPlaneACL(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v4beta/lke/clusters/7/control_plane_acl" {
			t.Errorf("%s %s, want PUT .../control_plane_acl", r.Method, r.URL.Path)
		}
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		acl, ok := got["acl"].(map[string]any)
		if !ok {
			t.Fatalf("body missing acl envelope: %v", got)
		}
		if acl["enabled"] != true {
			t.Errorf("enabled = %v, want true", acl["enabled"])
		}
		w.WriteHeader(http.StatusOK)
	})
	err := c.PutControlPlaneACL(context.Background(), 7, ControlPlaneACL{Enabled: true, IPv4: []string{"1.1.1.1/32"}})
	if err != nil {
		t.Errorf("PutControlPlaneACL = %v, want nil", err)
	}
}

func TestPutControlPlaneACLError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	})
	if err := c.PutControlPlaneACL(context.Background(), 7, ControlPlaneACL{}); err == nil {
		t.Error("PutControlPlaneACL on 403 = nil, want error")
	}
}
