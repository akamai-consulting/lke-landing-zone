package main

import (
	"context"
	"encoding/json"
	"errors"
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

// The GitHub target list is what decides which PATs reach the credential single
// pane at all. It used to be two literals inline at the call site, so
// E2E_DISPATCH_TOKEN and GHCR_READ_TOKEN — both ordinary PATs with a readable
// expiry — were structurally unmeasurable.
func TestGHTargetsFromEnvIncludesTheOptionalPATsWhenSet(t *testing.T) {
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "ghp-openbao")
	t.Setenv("APL_VALUES_REPO_TOKEN", "ghp-aplvalues")
	t.Setenv("E2E_DISPATCH_TOKEN", "ghp-e2e")
	t.Setenv("GHCR_READ_TOKEN", "ghp-ghcr")

	got := ghTargetsFromEnv("https://api.example.test")
	if len(got) != 4 {
		t.Fatalf("got %d targets, want all 4: %+v", len(got), got)
	}
	byName := map[string]patTarget{}
	for _, g := range got {
		byName[g.name] = g
		if g.api != "https://api.example.test" {
			t.Errorf("%s: api = %q, want the injected base", g.name, g.api)
		}
	}
	for name, want := range map[string]string{
		"OPENBAO_SECRETS_WRITE_TOKEN": "ghp-openbao",
		"APL_VALUES_REPO_TOKEN":       "ghp-aplvalues",
		"E2E_DISPATCH_TOKEN":          "ghp-e2e",
		"GHCR_READ_TOKEN":             "ghp-ghcr",
	} {
		if byName[name].token != want {
			t.Errorf("%s: token = %q, want %q", name, byName[name].token, want)
		}
	}
}

// An unset OPTIONAL PAT is dropped, not reported as unknown. Most instances set
// neither, and gatherGitHubTokens turns an empty token into a state=unknown row —
// so keeping them would put two permanent "unknown" entries on every stock
// instance's dashboard, which is how an inventory stops being read.
func TestGHTargetsFromEnvDropsUnsetOptionalPATs(t *testing.T) {
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "ghp-openbao")
	t.Setenv("APL_VALUES_REPO_TOKEN", "ghp-aplvalues")
	t.Setenv("E2E_DISPATCH_TOKEN", "")
	t.Setenv("GHCR_READ_TOKEN", "")

	got := ghTargetsFromEnv("https://api.github.com")
	if len(got) != 2 {
		t.Fatalf("got %d targets, want only the 2 required: %+v", len(got), got)
	}
	for _, g := range got {
		if g.name == "E2E_DISPATCH_TOKEN" || g.name == "GHCR_READ_TOKEN" {
			t.Errorf("unset optional target %s must be dropped, not measured", g.name)
		}
	}
}

// A REQUIRED PAT that is unset must still be reported — its absence is a finding,
// not a non-event. This is the asymmetry the `optional` field exists to express;
// dropping both kinds would hide a missing bootstrap credential.
func TestGHTargetsFromEnvKeepsUnsetRequiredPATs(t *testing.T) {
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "")
	t.Setenv("APL_VALUES_REPO_TOKEN", "")
	t.Setenv("E2E_DISPATCH_TOKEN", "")
	t.Setenv("GHCR_READ_TOKEN", "")

	got := ghTargetsFromEnv("https://api.github.com")
	if len(got) != 2 {
		t.Fatalf("got %d targets, want the 2 required ones even when unset: %+v", len(got), got)
	}
	// And they must classify as unknown (not ok) so nothing reads as healthy.
	for _, e := range gatherGitHubTokens(got, time.Unix(1_800_000_000, 0), 90, 14) {
		if e.State != tokenStateUnknown {
			t.Errorf("%s unset: state = %q, want %q", e.Name, e.State, tokenStateUnknown)
		}
	}
}

// The state-backend credentials have no expiry to read and cannot live in
// OpenBao (circular — OpenBao runs inside the cluster whose state they guard),
// so their only age signal is the GitHub secret's WRITE time. See ADR 0009.
func TestGatherSecretAgesRecordsWriteTime(t *testing.T) {
	got := gatherSecretAges("infra-primary", func(env, name string) (string, bool, error) {
		if env == "infra-primary" {
			return "2026-05-01T10:00:00Z", true, nil
		}
		return "", false, nil
	})
	if len(got) != len(ghSecretTargets) {
		t.Fatalf("got %d entries, want %d", len(got), len(ghSecretTargets))
	}
	for _, e := range got {
		if e.UpdatedAt != "2026-05-01T10:00:00Z" || e.State != tokenStateOK || e.Scope != "infra-primary" {
			t.Errorf("%s: got %+v", e.Name, e)
		}
	}
}

// An ABSENT secret must be reported as unknown, not dropped. This is the property
// the OpenBao age sampler cannot provide — there a 404 means "not seeded yet" and
// is skipped, so a never-written credential looks exactly like a healthy one.
// Here the API distinguishes them, so a missing state-backend credential stays
// visible rather than silently absent.
func TestGatherSecretAgesReportsAbsentAsUnknown(t *testing.T) {
	got := gatherSecretAges("infra-primary", func(string, string) (string, bool, error) {
		return "", false, nil
	})
	if len(got) != len(ghSecretTargets) {
		t.Fatalf("absent secrets must still be reported, got %d", len(got))
	}
	for _, e := range got {
		if e.State != tokenStateUnknown || e.UpdatedAt != "" {
			t.Errorf("%s: want unknown with no timestamp, got %+v", e.Name, e)
		}
	}
}

// Env scope is tried first, then repo — an instance may hold either.
func TestGatherSecretAgesFallsBackToRepoScope(t *testing.T) {
	got := gatherSecretAges("infra-primary", func(env, name string) (string, bool, error) {
		if env == "" {
			return "2026-01-02T03:04:05Z", true, nil
		}
		return "", false, nil // not in the environment scope
	})
	for _, e := range got {
		if e.Scope != "repo" || e.UpdatedAt == "" {
			t.Errorf("%s: want the repo-scope hit, got %+v", e.Name, e)
		}
	}
}

// A probe error must not lose the other providers' entries — the inventory is
// best-effort per source, and a wholesale funnel break is covered by
// LLZTokenInventoryStale rather than by failing the job.
func TestGatherSecretAgesToleratesProbeErrors(t *testing.T) {
	got := gatherSecretAges("infra-primary", func(string, string) (string, bool, error) {
		return "", false, errors.New("403")
	})
	if len(got) != len(ghSecretTargets) {
		t.Fatalf("probe errors must not drop entries, got %d", len(got))
	}
	for _, e := range got {
		if e.State != tokenStateUnknown {
			t.Errorf("%s: an unreadable secret is unknown, not ok", e.Name)
		}
	}
}

func TestSecretScopeForRegion(t *testing.T) {
	if got := secretScopeForRegion("primary"); got != "infra-primary" {
		t.Errorf("got %q, want infra-primary", got)
	}
	if got := secretScopeForRegion("  "); got != "" {
		t.Errorf("blank region should mean repo scope, got %q", got)
	}
}

// The class strings on ghSecretTargets are literals (the credClass* constants
// ship with the credential-coverage PR). Pin the vocabulary so a typo cannot
// publish a class no alert rule matches — which would be silently inert.
func TestGHSecretTargetClassesAreKnown(t *testing.T) {
	known := map[string]bool{
		"automated": true, "on-demand": true, "generate-once": true,
		"tracks-source": true, "static": true,
	}
	for _, tgt := range ghSecretTargets {
		if !known[tgt.class] {
			t.Errorf("%s has class %q, which no alert rule matches", tgt.name, tgt.class)
		}
	}
}
