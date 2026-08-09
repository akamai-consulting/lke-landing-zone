package tokeninv

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credpaths"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credtargets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

// a fixed "now" so expiry math is deterministic.
var tiNow = time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

// fake CredLister for the Linode half.
type fakeCredLister struct{ tokens []map[string]any }

func (f fakeCredLister) ListProfileTokens(context.Context) ([]map[string]any, error) {
	return f.tokens, nil
}
func (f fakeCredLister) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return nil, nil
}

func TestGatherGitHubTokens(t *testing.T) {
	// Stub the probe: name → (code, expHeader).
	orig := tokenprobe.GHPATProbe
	defer func() { tokenprobe.GHPATProbe = orig }()
	resp := map[string]struct {
		code int
		hdr  string
	}{
		"ok-token":       {200, "2026-08-05 00:00:00 UTC"}, // ~30d out → ok
		"soon-token":     {200, "2026-07-13 00:00:00 UTC"}, // 7d → warn
		"noexpiry-token": {200, ""},                        // no header → breach
		"invalid-token":  {401, ""},                        // 401 → breach
	}
	tokenprobe.GHPATProbe = func(_, token string) (int, string, error) {
		r := resp[token]
		return r.code, r.hdr, nil
	}
	targets := []PATTarget{
		{"ok", "https://api", "ok-token"},
		{"soon", "https://api", "soon-token"},
		{"noexp", "https://api", "noexpiry-token"},
		{"invalid", "https://api", "invalid-token"},
		{"missing", "https://api", ""}, // not set → unknown
	}
	got := gatherGitHubTokens(targets, tiNow, 90, 14)
	byName := map[string]credtargets.TokenEntry{}
	for _, e := range got {
		byName[e.Name] = e
	}
	if byName["ok"].State != credtargets.TokenStateOK || byName["ok"].Expiry == 0 {
		t.Errorf("ok token: %+v", byName["ok"])
	}
	if byName["soon"].State != credtargets.TokenStateWarn {
		t.Errorf("soon token should be warn: %+v", byName["soon"])
	}
	if byName["noexp"].State != credtargets.TokenStateBreach {
		t.Errorf("no-expiry token should be breach: %+v", byName["noexp"])
	}
	if byName["invalid"].State != credtargets.TokenStateBreach {
		t.Errorf("401 token should be breach: %+v", byName["invalid"])
	}
	if byName["missing"].State != credtargets.TokenStateUnknown {
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
	byName := map[string]credtargets.TokenEntry{}
	for _, e := range got {
		if e.Provider != "linode" {
			t.Errorf("provider should be linode: %+v", e)
		}
		byName[e.Name] = e
	}
	if byName["1:ok-pat"].State != credtargets.TokenStateOK {
		t.Errorf("ok-pat: %+v", byName["1:ok-pat"])
	}
	if byName["2:soon-pat"].State != credtargets.TokenStateWarn {
		t.Errorf("soon-pat should be warn: %+v", byName["2:soon-pat"])
	}
	if byName["3:noexp-pat"].State != credtargets.TokenStateBreach || byName["3:noexp-pat"].Expiry != 0 {
		t.Errorf("noexp-pat should be breach with 0 expiry: %+v", byName["3:noexp-pat"])
	}
	if byName["4:old-pat"].State != credtargets.TokenStateBreach {
		t.Errorf("old-pat should be breach: %+v", byName["4:old-pat"])
	}
}

func TestBuildTokenInventorySortedAndStamped(t *testing.T) {
	orig := tokenprobe.GHPATProbe
	defer func() { tokenprobe.GHPATProbe = orig }()
	tokenprobe.GHPATProbe = func(_, _ string) (int, string, error) { return 200, "2026-09-01 00:00:00 UTC", nil }
	inv := buildTokenInventory(context.Background(), tokenInvDeps{
		ghTargets:   []PATTarget{{"zzz", "https://api", "t"}, {"aaa", "https://api", "t"}},
		linodeToken: "tok",
		newLinode: func(string) CredLister {
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
	inv := credtargets.Inventory{Updated: 1720000000, Region: "primary", Tokens: []credtargets.TokenEntry{
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
	var back credtargets.Inventory
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
	byName := map[string]PATTarget{}
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
		if e.State != credtargets.TokenStateUnknown {
			t.Errorf("%s unset: state = %q, want %q", e.Name, e.State, credtargets.TokenStateUnknown)
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
	if len(got) != len(credtargets.GHSecretTargets) {
		t.Fatalf("got %d entries, want %d", len(got), len(credtargets.GHSecretTargets))
	}
	for _, e := range got {
		if e.UpdatedAt != "2026-05-01T10:00:00Z" || e.State != credtargets.TokenStateOK || e.Scope != "infra-primary" {
			t.Errorf("%s: got %+v", e.Name, e)
		}
	}
}

// An ABSENT secret must be reported, not dropped — the property the OpenBao age
// sampler cannot provide, since there a 404 means "not seeded yet" and is
// skipped, so a never-written credential looks exactly like a healthy one.
//
// It is reported as `absent`, NOT `unknown`. This test asserted `unknown` when
// ADR 0009 wrote it, and that was the conflation: `unknown` is also what an
// unreadable secret produces, so the two answers "the API says this does not
// exist" and "the API would not tell me" were one value. The reconciler turns
// the first into llz_credential_configured=0, which LLZCredentialUnconfigured
// reads as "seed this credential" — so publishing it for the second would page
// on a token-permission fault while naming a missing credential.
func TestGatherSecretAgesReportsAbsentDistinctlyFromUnreadable(t *testing.T) {
	absent := gatherSecretAges("infra-primary", func(string, string) (string, bool, error) {
		return "", false, nil // 404: the API answered
	})
	if len(absent) != len(credtargets.GHSecretTargets) {
		t.Fatalf("absent secrets must still be reported, got %d", len(absent))
	}
	for _, e := range absent {
		if e.State != credtargets.TokenStateAbsent || e.UpdatedAt != "" {
			t.Errorf("%s: want absent with no timestamp, got %+v", e.Name, e)
		}
	}

	unreadable := gatherSecretAges("infra-primary", func(string, string) (string, bool, error) {
		return "", false, errors.New("403 Forbidden") // the API refused
	})
	for _, e := range unreadable {
		if e.State != credtargets.TokenStateUnknown {
			t.Errorf("%s: a refused read must stay unknown, got %+v", e.Name, e)
		}
	}
}

// Found in one scope and refused in the other is still FOUND. The probe tries the
// environment scope then the repo scope, and those carry different permissions —
// so downgrading on any error seen would report a credential we successfully read
// as unreadable, and take the whole lane's funnel verdict down with it.
func TestGatherSecretAgesPrefersAFindOverALaterRefusal(t *testing.T) {
	got := gatherSecretAges("infra-primary", func(env, name string) (string, bool, error) {
		if env == "infra-primary" {
			return "2026-05-01T10:00:00Z", true, nil
		}
		return "", false, errors.New("403 Forbidden")
	})
	for _, e := range got {
		if e.State != credtargets.TokenStateOK {
			t.Errorf("%s: a successful read must win, got %+v", e.Name, e)
		}
	}
}

// The funnel verdict must fall when ANY credential went unread, not only when the
// client could not be built. A client that authenticates for repo-scoped secrets
// can still be refused on the environment scope — different permissions — and the
// five OpenBao credentials measured here are environment-scoped. Reporting `ok`
// there would vouch for a lane that is partly dark.
func TestSecretProbeVerdictFallsOnAPerCredentialRefusal(t *testing.T) {
	ok := []credtargets.SecretEntry{{State: credtargets.TokenStateOK}, {State: credtargets.TokenStateAbsent}}
	if got := credtargets.SecretProbeVerdict(true, ok); got != credtargets.SecretProbeOK {
		t.Errorf("answered entries = %q, want %q", got, credtargets.SecretProbeOK)
	}
	partial := []credtargets.SecretEntry{{State: credtargets.TokenStateOK}, {State: credtargets.TokenStateUnknown}}
	if got := credtargets.SecretProbeVerdict(true, partial); got != credtargets.SecretProbeUnavailable {
		t.Errorf("one unreadable entry = %q, want %q", got, credtargets.SecretProbeUnavailable)
	}
	if got := credtargets.SecretProbeVerdict(false, nil); got != credtargets.SecretProbeUnavailable {
		t.Errorf("no client = %q, want %q", got, credtargets.SecretProbeUnavailable)
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
	if len(got) != len(credtargets.GHSecretTargets) {
		t.Fatalf("probe errors must not drop entries, got %d", len(got))
	}
	for _, e := range got {
		if e.State != credtargets.TokenStateUnknown {
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

// These series ride the SAME metric and label vocabulary as the OpenBao age
// sampler, so a class outside the known set would publish a series no alert rule
// matches — silently inert, which is the exact failure the class label exists to
// prevent. Pinned against the constants themselves, so renaming one breaks here
// rather than in production.
func TestGHSecretTargetClassesAreKnown(t *testing.T) {
	known := map[string]bool{
		credpaths.CredClassAutomated: true, credpaths.CredClassOnDemand: true, credpaths.CredClassGenerateOnce: true,
		credpaths.CredClassTracksSource: true, credpaths.CredClassStatic: true,
	}
	for _, tgt := range credtargets.GHSecretTargets {
		if !known[tgt.Class] {
			t.Errorf("%s has class %q, which no alert rule matches", tgt.Name, tgt.Class)
		}
	}
}

// Every target must declare an `expect` from the closed set. An empty one is not
// a neutral default: the reconciler substitutes `present`, so a forgotten field
// silently promises that a credential must exist — which for a root token is
// exactly backwards, and for the Harbor pair pages every healthy standby.
func TestGHSecretTargetsDeclareExpect(t *testing.T) {
	known := map[string]bool{
		credtargets.CredExpectPresent: true, credtargets.CredExpectOptional: true, credtargets.CredExpectAbsent: true,
	}
	for _, tgt := range credtargets.GHSecretTargets {
		if !known[tgt.Expect] {
			t.Errorf("%s: expect = %q, outside the closed set", tgt.Name, tgt.Expect)
		}
	}
}

// The credentials this measurement exists for. ADR 0009 built the write-time
// probe for the state backend and stopped there; the seal key, the recovery
// quorum, the root token and the Harbor robot copies have no expiry either, are
// circular with OpenBao for exactly the same reason, and were simply not in the
// list it wrote. Pinned by name so removing one is a deliberate edit here rather
// than a credential quietly leaving the single pane.
func TestGHSecretTargetsCoverOpenBaoEscrowAndHarborStandby(t *testing.T) {
	got := map[string]string{}
	for _, tgt := range credtargets.GHSecretTargets {
		got[tgt.Name] = tgt.Expect
	}
	for _, name := range []string{
		"OPENBAO_SEAL_KEY",       // the at-rest key for everything else in OpenBao
		"OPENBAO_RECOVERY_KEY_1", // lose the quorum and break-glass is impossible
		"OPENBAO_RECOVERY_KEY_2",
		"OPENBAO_RECOVERY_KEY_3",
	} {
		if got[name] != credtargets.CredExpectPresent {
			t.Errorf("%s must be measured and expected present, got expect=%q", name, got[name])
		}
	}
	// The Harbor robot pair is measured too, but OPTIONAL: it is published by the
	// ACTIVE peer's provisioner, so a standby peer has neither until that has run.
	// Classing it `present` pages every healthy standby and fails its daily
	// credential job — a gap closed by a rule that cries wolf is not closed.
	for _, name := range []string{"HARBOR_PASSWORD", "HARBOR_PULL_PASSWORD"} {
		if got[name] != credtargets.CredExpectOptional {
			t.Errorf("%s must be expect=optional, got %q", name, got[name])
		}
	}
	// The one credential whose healthy state is absent. Bootstrap mints a root
	// token, uses it and revokes it; a set one is a live full-admin credential
	// left behind by a break-glass that never ran its revoke.
	if got["OPENBAO_ROOT_TOKEN"] != credtargets.CredExpectAbsent {
		t.Errorf("OPENBAO_ROOT_TOKEN must be expect=absent, got %q", got["OPENBAO_ROOT_TOKEN"])
	}
}

// The expect verdict has to reach the ConfigMap; the reconciler reads it from
// there and nowhere else.
func TestGatherSecretAgesCarriesExpect(t *testing.T) {
	want := map[string]string{}
	for _, tgt := range credtargets.GHSecretTargets {
		want[tgt.Name] = tgt.Expect
	}
	for _, e := range gatherSecretAges("infra-primary", func(string, string) (string, bool, error) {
		return "", false, nil
	}) {
		if e.Expect != want[e.Name] {
			t.Errorf("%s: entry carries expect=%q, target declares %q", e.Name, e.Expect, want[e.Name])
		}
	}
}

// An empty Secrets list is ambiguous — "measured, none exist" and "the probe
// never ran" render identically — and the ambiguity was not theoretical: the one
// job that runs this command supplied a token but no GH_REPO, so the probe
// errored on every run since ADR 0009 shipped and no write-time series was ever
// published. The verdict has to be explicit for the reconciler to alert on it.
func TestBuildTokenInventoryRecordsSecretProbeUnavailable(t *testing.T) {
	inv := buildTokenInventory(context.Background(), tokenInvDeps{
		secretEnv:   "infra-primary",
		secretProbe: nil, // newSecretAgeWriter failed — the production case
		now:         time.Unix(1_800_000_000, 0),
	})
	if inv.SecretProbe != credtargets.SecretProbeUnavailable {
		t.Errorf("SecretProbe = %q, want %q", inv.SecretProbe, credtargets.SecretProbeUnavailable)
	}
	if len(inv.Secrets) != 0 {
		t.Errorf("an unavailable probe must contribute no entries, got %d", len(inv.Secrets))
	}
}

func TestBuildTokenInventoryRecordsSecretProbeOK(t *testing.T) {
	inv := buildTokenInventory(context.Background(), tokenInvDeps{
		secretEnv: "infra-primary",
		secretProbe: func(string, string) (string, bool, error) {
			return "2026-05-01T00:00:00Z", true, nil
		},
		now: time.Unix(1_800_000_000, 0),
	})
	if inv.SecretProbe != credtargets.SecretProbeOK {
		t.Errorf("SecretProbe = %q, want %q", inv.SecretProbe, credtargets.SecretProbeOK)
	}
	if len(inv.Secrets) != len(credtargets.GHSecretTargets) {
		t.Errorf("got %d entries, want %d", len(inv.Secrets), len(credtargets.GHSecretTargets))
	}
}
