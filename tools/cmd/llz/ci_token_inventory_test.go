package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

func TestLinodeManagedBy(t *testing.T) {
	cases := map[string]string{
		"llz-dns-us-ord-1":  "cred-rotator",
		"loki-us-ord-1":     "cred-rotator",
		"harbor-registry":   "cred-rotator",
		"LLZ-Upper":         "cred-rotator", // case-insensitive
		"my-personal-token": "manual",
		"<unlabelled>":      "manual",
	}
	for label, want := range cases {
		if got := linodeManagedBy(label); got != want {
			t.Errorf("linodeManagedBy(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestGHTokenStatus(t *testing.T) {
	cases := map[health.PATCheckState]string{
		health.PATOK:          "ok",
		health.PATWarn:        "warn",
		health.PATExpired:     "breach",
		health.PATNoExpiry:    "breach",
		health.PATInvalid:     "breach",
		health.PATOverPolicy:  "breach",
		health.PATNotSet:      "unknown",
		health.PATUnreachable: "unknown",
		health.PATUnparseable: "unknown",
	}
	for state, want := range cases {
		if got := ghTokenStatus(state); got != want {
			t.Errorf("ghTokenStatus(%v) = %q, want %q", state, got, want)
		}
	}
}

func TestGatherLinodeTokens(t *testing.T) {
	prev := newCredAuditClient
	t.Cleanup(func() { newCredAuditClient = prev })
	newCredAuditClient = func(string) credLister {
		return fakeLister{
			tokens: []map[string]any{
				{"id": uint64(1), "label": "llz-dns-us-ord", "created": ts(-10), "expiry": ts(80)}, // ok
				{"id": uint64(2), "label": "soon", "created": ts(-10), "expiry": ts(3)},            // warn (<14d)
				{"id": uint64(3), "label": "no-expiry"},                                            // breach (no expiry)
				{"id": uint64(4), "label": "stale", "created": ts(-200), "expiry": ts(-1)},         // breach (expired)
			},
			keys: []map[string]any{
				{"id": uint64(9), "label": "loki-us-ord"}, // static obj-key
			},
		}
	}

	got, err := gatherLinodeTokens(context.Background(), "tok", time.Now().Unix(), 90, 14)
	if err != nil {
		t.Fatalf("gatherLinodeTokens: %v", err)
	}
	byName := map[string]tokenEntry{}
	for _, e := range got {
		byName[e.name] = e
	}
	if e := byName["llz-dns-us-ord"]; e.status != "ok" || !e.hasExpiry {
		t.Errorf("llz-dns-us-ord = %+v, want ok+expiry", e)
	}
	if e := byName["soon"]; e.status != "warn" {
		t.Errorf("soon status = %q, want warn", e.status)
	}
	if e := byName["no-expiry"]; e.status != "breach" || e.hasExpiry {
		t.Errorf("no-expiry = %+v, want breach without expiry", e)
	}
	if e := byName["stale"]; e.status != "breach" {
		t.Errorf("stale status = %q, want breach", e.status)
	}
	if e := byName["loki-us-ord"]; e.kind != "linode-obj-key" || e.status != "static" || e.managedBy != "cred-rotator" {
		t.Errorf("loki-us-ord = %+v, want obj-key/static/cred-rotator", e)
	}
}

func TestRenderTokenMetrics(t *testing.T) {
	entries := []tokenEntry{
		{name: "openbao-seal-key", kind: "seal-key", source: "openbao", managedBy: "bootstrap", status: "static"},
		{name: `quote"and\slash`, kind: "github-pat", source: "github", managedBy: "manual", status: "warn", expiry: 1234567890, hasExpiry: true},
	}
	var buf bytes.Buffer
	renderTokenMetrics(&buf, entries, 1700000000)
	out := buf.String()

	for _, want := range []string{
		"# TYPE llz_token_inventory_info gauge",
		`llz_token_inventory_info{name="openbao-seal-key",kind="seal-key",source="openbao",managed_by="bootstrap"} 1`,
		`llz_token_audit_status{name="openbao-seal-key",kind="seal-key",source="openbao",managed_by="bootstrap",status="static"} 1`,
		`llz_token_expiry_timestamp_seconds{name="quote\"and\\slash",kind="github-pat",source="github",managed_by="manual"} 1234567890`,
		"llz_token_inventory_push_timestamp_seconds 1700000000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered metrics missing line:\n  %s\n--- full output ---\n%s", want, out)
		}
	}

	// The static seal key has no expiry, so it must NOT emit an expiry series.
	if strings.Contains(out, `llz_token_expiry_timestamp_seconds{name="openbao-seal-key"`) {
		t.Error("static entry without expiry should not emit an expiry timestamp series")
	}
}

func TestEscLabel(t *testing.T) {
	if got := escLabel(`a"b\c` + "\n"); got != `a\"b\\c\n` {
		t.Errorf("escLabel = %q", got)
	}
}

func TestStaticTokenInventory(t *testing.T) {
	got := staticTokenInventory()
	if len(got) == 0 {
		t.Fatal("staticTokenInventory is empty")
	}
	for _, e := range got {
		if e.status != "static" || e.hasExpiry {
			t.Errorf("static entry %q must be status=static without expiry, got %+v", e.name, e)
		}
	}
}

func TestGatherGitHubTokens(t *testing.T) {
	orig := ghPATProbe
	t.Cleanup(func() { ghPATProbe = orig })
	// Token present + an expiry ~30 days out (inside the >warn, <max window) → ok.
	// A far-future expiry would be PATOverPolicy (breach) under the ≤90-day policy.
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "x")
	t.Setenv("APL_VALUES_REPO_TOKEN", "") // absent → status=unknown, no probe
	okExpiry := time.Now().Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05 UTC")
	ghPATProbe = func(_, _ string) (int, string, error) {
		return 200, okExpiry, nil
	}

	got := gatherGitHubTokens(time.Now(), 90, 14)
	byName := map[string]tokenEntry{}
	for _, e := range got {
		byName[e.name] = e
	}
	if e := byName["OPENBAO_SECRETS_WRITE_TOKEN"]; e.status != "ok" || !e.hasExpiry || e.kind != "github-pat" {
		t.Errorf("OPENBAO_SECRETS_WRITE_TOKEN = %+v, want ok/github-pat/expiry", e)
	}
	if e := byName["APL_VALUES_REPO_TOKEN"]; e.status != "unknown" {
		t.Errorf("absent APL_VALUES_REPO_TOKEN status = %q, want unknown", e.status)
	}
}

func TestRunCITokenInventoryToFile(t *testing.T) {
	// Force every external source off so the run is hermetic: no Linode token,
	// no GitHub tokens (so ghPATProbe is never called).
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "")
	t.Setenv("APL_VALUES_REPO_TOKEN", "")

	out := filepath.Join(t.TempDir(), "metrics.txt")
	if err := runCITokenInventory(tokenInventoryOpts{maxPATDays: 90, warnDays: 14, output: out}); err != nil {
		t.Fatalf("runCITokenInventory: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `llz_token_inventory_info{name="openbao-seal-key"`) {
		t.Error("output missing the static seal-key inventory entry")
	}
	if !strings.Contains(s, "llz_token_inventory_push_timestamp_seconds ") {
		t.Error("output missing the freshness heartbeat")
	}
}
