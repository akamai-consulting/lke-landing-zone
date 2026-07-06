package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// a fixed "now" so expiry math is deterministic.
var tiNow = time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

// fake credLister for the Linode half.
type fakeCredLister struct{ tokens []map[string]any }

func (f fakeCredLister) ListProfileTokens(context.Context) ([]map[string]any, error) {
	return f.tokens, nil
}
func (f fakeCredLister) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return nil, nil
}

func TestGatherGitHubTokens(t *testing.T) {
	// Stub the probe: name → (code, expHeader).
	orig := ghPATProbe
	defer func() { ghPATProbe = orig }()
	resp := map[string]struct {
		code int
		hdr  string
	}{
		"ok-token":       {200, "2026-08-05 00:00:00 UTC"}, // ~30d out → ok
		"soon-token":     {200, "2026-07-13 00:00:00 UTC"}, // 7d → warn
		"noexpiry-token": {200, ""},                        // no header → breach
		"invalid-token":  {401, ""},                        // 401 → breach
	}
	ghPATProbe = func(_, token string) (int, string, error) {
		r := resp[token]
		return r.code, r.hdr, nil
	}
	targets := []patTarget{
		{"ok", "https://api", "ok-token"},
		{"soon", "https://api", "soon-token"},
		{"noexp", "https://api", "noexpiry-token"},
		{"invalid", "https://api", "invalid-token"},
		{"missing", "https://api", ""}, // not set → unknown
	}
	got := gatherGitHubTokens(targets, tiNow, 90, 14)
	byName := map[string]tokenEntry{}
	for _, e := range got {
		byName[e.Name] = e
	}
	if byName["ok"].State != tokenStateOK || byName["ok"].Expiry == 0 {
		t.Errorf("ok token: %+v", byName["ok"])
	}
	if byName["soon"].State != tokenStateWarn {
		t.Errorf("soon token should be warn: %+v", byName["soon"])
	}
	if byName["noexp"].State != tokenStateBreach {
		t.Errorf("no-expiry token should be breach: %+v", byName["noexp"])
	}
	if byName["invalid"].State != tokenStateBreach {
		t.Errorf("401 token should be breach: %+v", byName["invalid"])
	}
	if byName["missing"].State != tokenStateUnknown {
		t.Errorf("unset token should be unknown: %+v", byName["missing"])
	}
	for _, e := range got {
		if e.Provider != "github" {
			t.Errorf("provider should be github: %+v", e)
		}
	}
}

func TestGatherLinodeTokens(t *testing.T) {
	client := fakeCredLister{tokens: []map[string]any{
		{"id": float64(1), "label": "ok-pat", "created": "2026-06-01T00:00:00", "expiry": "2026-08-01T00:00:00"},   // ~26d → ok
		{"id": float64(2), "label": "soon-pat", "created": "2026-06-01T00:00:00", "expiry": "2026-07-13T00:00:00"}, // 7d → warn
		{"id": float64(3), "label": "noexp-pat", "created": "2026-06-01T00:00:00", "expiry": ""},                   // no expiry → breach
		{"id": float64(4), "label": "old-pat", "created": "2026-01-01T00:00:00", "expiry": "2026-06-01T00:00:00"},  // already expired → breach
	}}
	got, err := gatherLinodeTokens(context.Background(), client, tiNow, 90, 14)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]tokenEntry{}
	for _, e := range got {
		if e.Provider != "linode" {
			t.Errorf("provider should be linode: %+v", e)
		}
		byName[e.Name] = e
	}
	if byName["1:ok-pat"].State != tokenStateOK {
		t.Errorf("ok-pat: %+v", byName["1:ok-pat"])
	}
	if byName["2:soon-pat"].State != tokenStateWarn {
		t.Errorf("soon-pat should be warn: %+v", byName["2:soon-pat"])
	}
	if byName["3:noexp-pat"].State != tokenStateBreach || byName["3:noexp-pat"].Expiry != 0 {
		t.Errorf("noexp-pat should be breach with 0 expiry: %+v", byName["3:noexp-pat"])
	}
	if byName["4:old-pat"].State != tokenStateBreach {
		t.Errorf("old-pat should be breach: %+v", byName["4:old-pat"])
	}
}

func TestBuildTokenInventorySortedAndStamped(t *testing.T) {
	orig := ghPATProbe
	defer func() { ghPATProbe = orig }()
	ghPATProbe = func(_, _ string) (int, string, error) { return 200, "2026-09-01 00:00:00 UTC", nil }
	inv := buildTokenInventory(context.Background(), tokenInvDeps{
		ghTargets:   []patTarget{{"zzz", "https://api", "t"}, {"aaa", "https://api", "t"}},
		linodeToken: "tok",
		newLinode: func(string) credLister {
			return fakeCredLister{tokens: []map[string]any{{"id": float64(9), "label": "l", "expiry": "2026-09-01T00:00:00"}}}
		},
		region:   "primary",
		now:      tiNow,
		maxDays:  90,
		warnDays: 14,
	})
	if inv.Updated != tiNow.Unix() || inv.Region != "primary" {
		t.Errorf("stamp wrong: %+v", inv)
	}
	// sorted by provider then name: github/aaa, github/zzz, linode/9:l
	if len(inv.Tokens) != 3 || inv.Tokens[0].Name != "aaa" || inv.Tokens[1].Name != "zzz" || inv.Tokens[2].Provider != "linode" {
		t.Errorf("unsorted: %+v", inv.Tokens)
	}
}

func TestRenderInventoryConfigMapNoTokenValues(t *testing.T) {
	inv := tokenInventory{Updated: 1720000000, Region: "primary", Tokens: []tokenEntry{
		{Provider: "github", Name: "APL_VALUES_REPO_TOKEN", Expiry: 1725000000, State: "ok"},
	}}
	out, err := renderInventoryConfigMap(inv, "llz-reconciler", "llz-token-inventory")
	if err != nil {
		t.Fatal(err)
	}
	var cm map[string]any
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if cm["kind"] != "ConfigMap" {
		t.Errorf("not a ConfigMap: %v", cm["kind"])
	}
	meta := cm["metadata"].(map[string]any)
	if meta["name"] != "llz-token-inventory" || meta["namespace"] != "llz-reconciler" {
		t.Errorf("metadata wrong: %v", meta)
	}
	// The embedded inventory round-trips.
	data := cm["data"].(map[string]any)
	var back tokenInventory
	if err := json.Unmarshal([]byte(data["inventory.json"].(string)), &back); err != nil {
		t.Fatalf("embedded inventory not JSON: %v", err)
	}
	if len(back.Tokens) != 1 || back.Tokens[0].Name != "APL_VALUES_REPO_TOKEN" {
		t.Errorf("round-trip lost data: %+v", back)
	}
	// A token VALUE must never appear in the rendered ConfigMap.
	if strings.Contains(out, "ghp_") || strings.Contains(strings.ToLower(out), "secret") {
		t.Errorf("rendered ConfigMap must carry no token material:\n%s", out)
	}
}
