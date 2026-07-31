package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// fakeGetter satisfies nodeGetter (GetJSON) with a canned object + status.
type fakeGetter struct {
	obj    map[string]any
	status int
}

func (f fakeGetter) GetJSON(context.Context, string) (map[string]any, int, error) {
	return f.obj, f.status, nil
}

func metricsDump(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestSampleTokenInventoryExposesGauges(t *testing.T) {
	cm := map[string]any{
		"data": map[string]any{
			"inventory.json": `{"updated":1720000000,"region":"primary","tokens":[
			  {"provider":"github","name":"APL_VALUES_REPO_TOKEN","expiry":1725000000,"state":"ok"},
			  {"provider":"linode","name":"9:pat","expiry":0,"state":"breach"},
			  {"provider":"github","name":"warn-tok","expiry":1724000000,"state":"warn"}
			]}`,
		},
	}
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	out := metricsDump(t, reg)
	for _, want := range []string{
		// timestamp values render in shortest-exact form (scientific notation is
		// fine — Prometheus parses it); assert the series exists, plus plain counts.
		`llz_token_inventory_updated_timestamp_seconds 1.72e+09`,
		`llz_token_inventory_tokens 3`,
		`llz_token_expiry_timestamp_seconds{provider="github",token="APL_VALUES_REPO_TOKEN"} `,
		`llz_token_audit_ok{provider="github",token="APL_VALUES_REPO_TOKEN"} 1`,
		`llz_token_audit_ok{provider="linode",token="9:pat"} 0`, // breach → 0
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	// A breach/0-expiry token must NOT emit an expiry gauge (expiry unknown).
	if strings.Contains(out, `llz_token_expiry_timestamp_seconds{provider="linode",token="9:pat"}`) {
		t.Errorf("0-expiry token should not emit an expiry gauge:\n%s", out)
	}
}

// A 404 (the writer job hasn't run yet) is a clean no-op, not an error.
func TestSampleTokenInventoryAbsentIsNoOp(t *testing.T) {
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: nil, status: 404}, reg, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatalf("404 should be a no-op, got %v", err)
	}
	if strings.Contains(metricsDump(t, reg), "llz_token_") {
		t.Error("no token metrics should be published when the ConfigMap is absent")
	}
}

// A present-but-empty ConfigMap (no inventory.json) is also a clean no-op.
func TestSampleTokenInventoryEmptyIsNoOp(t *testing.T) {
	reg := metrics.NewRegistry()
	cm := map[string]any{"data": map[string]any{}}
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatalf("empty ConfigMap should be a no-op, got %v", err)
	}
}

// The regression this whole series exists for: a credential the instance never
// configured published NOTHING. gatherSecretAges carried the distinction all the
// way here as an empty UpdatedAt, and the sampler dropped it before publishing —
// so a never-seeded recovery key or state passphrase was indistinguishable from a
// healthy one on the dashboard AND unreachable by every alert, because a rule over
// an absent series never evaluates. ADR 0009 believed the GitHub API had closed
// this ("the API distinguishes them, so no companion absence alert is needed"); it
// did distinguish them, and the reconciler threw the answer away.
func TestSampleTokenInventoryPublishesPresenceForAbsentSecrets(t *testing.T) {
	cm := map[string]any{
		"data": map[string]any{
			"inventory.json": `{"updated":1720000000,"tokens":[],"secrets":[
			  {"name":"TF_STATE_ACCESS_KEY","scope":"infra-primary","updated_at":"2026-05-01T00:00:00Z","class":"on-demand","expect":"present","state":"ok"},
			  {"name":"OPENBAO_RECOVERY_KEY_2","scope":"infra-primary","class":"static","expect":"present","state":"unknown"}
			]}`,
		},
	}
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	out := metricsDump(t, reg)
	for _, want := range []string{
		`llz_credential_configured{class="on-demand",cred="tf-state-access-key",expect="present"} 1`,
		`llz_credential_configured{class="static",cred="openbao-recovery-key-2",expect="present"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	// An absent credential still has no AGE — there is no value to have been
	// written. Presence is the only thing that can be said about it, which is
	// exactly why presence had to become its own series.
	if strings.Contains(out, `llz_credential_age_days{class="static",cred="openbao-recovery-key-2"}`) {
		t.Errorf("an absent secret must publish no age series:\n%s", out)
	}
}

// `expect` is a label rather than a filter because presence is not uniformly
// good: the root token is supposed to be ABSENT, so for that one a 1 is the
// finding. LLZRootTokenParked reads this series in the opposite direction from
// LLZCredentialUnconfigured, and it can only do that if the label survives.
func TestSampleTokenInventoryCarriesExpectAbsent(t *testing.T) {
	cm := map[string]any{
		"data": map[string]any{
			"inventory.json": `{"updated":1720000000,"tokens":[],"secrets":[
			  {"name":"OPENBAO_ROOT_TOKEN","scope":"infra-primary","updated_at":"2026-05-01T00:00:00Z","class":"on-demand","expect":"absent","state":"ok"}
			]}`,
		},
	}
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	if out := metricsDump(t, reg); !strings.Contains(out,
		`llz_credential_configured{class="on-demand",cred="openbao-root-token",expect="absent"} 1`) {
		t.Errorf("expect=absent must survive onto the series:\n%s", out)
	}
}

// The writer and the reader ship independently — the inventory ConfigMap is
// written by a CI job running whatever llz that instance's TF_IMAGE baked, and
// read by the in-cluster reconciler. An inventory from before `expect` existed
// carries none, and defaulting it to "absent" would make every credential in it
// fire LLZRootTokenParked. It defaults to `present`, so an old writer degrades to
// the pre-existing meaning instead of to a fleet-wide false page.
func TestSampleTokenInventoryDefaultsMissingExpectToPresent(t *testing.T) {
	cm := map[string]any{
		"data": map[string]any{
			"inventory.json": `{"updated":1720000000,"tokens":[],"secrets":[
			  {"name":"TF_STATE_SECRET_KEY","scope":"repo","updated_at":"2026-05-01T00:00:00Z","class":"on-demand","state":"ok"}
			]}`,
		},
	}
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	if out := metricsDump(t, reg); !strings.Contains(out, `expect="present"`) {
		t.Errorf("an inventory with no expect must default to present:\n%s", out)
	}
}

// The funnel, watched at its source. Both directions matter: a 0 must be
// published so LLZCredentialSecretProbeUnavailable can fire, and an inventory
// from a writer that predates the field must publish NEITHER value — guessing
// would either page an instance whose probe is fine or vouch for one whose isn't.
func TestSampleTokenInventoryPublishesSecretProbeVerdict(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want string
	}{
		{"unavailable", `{"updated":1,"tokens":[],"secret_probe":"unavailable"}`, `llz_credential_secret_probe_ok 0`},
		{"ok", `{"updated":1,"tokens":[],"secret_probe":"ok"}`, `llz_credential_secret_probe_ok 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := metrics.NewRegistry()
			cm := map[string]any{"data": map[string]any{"inventory.json": tc.payload}}
			if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
			if out := metricsDump(t, reg); !strings.Contains(out, tc.want) {
				t.Errorf("metrics missing %q:\n%s", tc.want, out)
			}
		})
	}

	reg := metrics.NewRegistry()
	cm := map[string]any{"data": map[string]any{"inventory.json": `{"updated":1,"tokens":[]}`}}
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if out := metricsDump(t, reg); strings.Contains(out, "llz_credential_secret_probe_ok") {
		t.Errorf("an inventory with no verdict must publish neither value:\n%s", out)
	}
}
