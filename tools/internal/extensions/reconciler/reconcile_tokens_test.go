package reconciler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
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
			  {"name":"OPENBAO_RECOVERY_KEY_2","scope":"infra-primary","class":"static","expect":"present","state":"absent"}
			]}`,
		},
	}
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	out := metricsDump(t, reg)
	for _, want := range []string{
		`llz_credential_configured{cred="tf-state-access-key"} 1`,
		`llz_credential_configured{cred="openbao-recovery-key-2"} 0`,
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
// finding. LLZCredentialRootTokenParked reads this series in the opposite direction from
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
	// The verdict, not the classification: a root token that IS set does not
	// match what is expected of it, and the pair of series is what the two rules
	// join on.
	out := metricsDump(t, reg)
	for _, want := range []string{
		`llz_credential_configured{cred="openbao-root-token"} 1`,
		`llz_credential_presence_ok{cred="openbao-root-token"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

// The writer and the reader ship independently — the inventory ConfigMap is
// written by a CI job running whatever llz that instance's TF_IMAGE baked, and
// read by the in-cluster reconciler. An inventory from before `expect` existed
// carries none, and defaulting it to "absent" would make every credential in it
// fire LLZCredentialRootTokenParked. It defaults to `present`, so an old writer degrades to
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
	// No `expect` from an old writer means "present", so a configured credential
	// satisfies it — asserted on the verdict, since the classification is applied
	// by the reconciler and never published as a label.
	if out := metricsDump(t, reg); !strings.Contains(out,
		`llz_credential_presence_ok{cred="tf-state-secret-key"} 1`) {
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

// The reconciler must not turn "I could not read it" into "it is not there".
// LLZCredentialUnconfigured reads llz_credential_configured=0 as "seed this
// credential", so publishing 0 for an unreadable secret pages on a token
// permission fault while naming a missing credential — and sends the operator to
// docs/secrets.md instead of to the token. The funnel gauge carries that case.
func TestSampleTokenInventoryPublishesNoPresenceForUnreadableSecrets(t *testing.T) {
	cm := map[string]any{
		"data": map[string]any{
			"inventory.json": `{"updated":1,"tokens":[],"secret_probe":"unavailable","secrets":[
			  {"name":"OPENBAO_SEAL_KEY","scope":"infra-primary","class":"static","expect":"present","state":"unknown"},
			  {"name":"TF_STATE_ACCESS_KEY","scope":"infra-primary","class":"on-demand","expect":"present","state":"absent"}
			]}`,
		},
	}
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	out := metricsDump(t, reg)
	if strings.Contains(out, `cred="openbao-seal-key"`) {
		t.Errorf("an unreadable secret must publish NO presence series:\n%s", out)
	}
	// The one the API actually answered for is still published — a partial
	// refusal must not blank the whole lane.
	if !strings.Contains(out, `llz_credential_configured{cred="tf-state-access-key"} 0`) {
		t.Errorf("an ANSWERED absence must still publish 0:\n%s", out)
	}
	// …and the funnel says the lane is degraded, so the silence above is not
	// mistaken for health.
	if !strings.Contains(out, "llz_credential_secret_probe_ok 0") {
		t.Errorf("the funnel verdict must carry the refusal:\n%s", out)
	}
}

// The defect this shape exists to prevent, reproduced against the real registry.
//
// tools/internal/metrics upserts keyed by the RENDERED LABEL SET and has no
// delete, so publishing a classification as a LABEL means a reclassification
// ADDS a series rather than replacing one — and the stale sample is served at its
// last value for the life of the pod. `expect` and `class` change when the
// WRITER's llz is upgraded, independently of this long-lived reconciler, and
// HARBOR_* moved present -> optional inside this very branch. Under the first
// shape that stranded a {expect="present"} 0 firing LLZCredentialUnconfigured
// forever until someone restarted the pod.
//
// Two passes over one registry with a changed classification must therefore leave
// exactly ONE series per credential.
func TestCredentialPresenceSurvivesReclassification(t *testing.T) {
	inv := func(expect string) map[string]any {
		return map[string]any{"data": map[string]any{"inventory.json": `{"updated":1,"tokens":[],"secret_probe":"ok","secrets":[
		  {"name":"HARBOR_PASSWORD","scope":"infra-primary","class":"static","expect":"` + expect + `","state":"absent"}
		]}`}}
	}
	reg := metrics.NewRegistry()
	for _, e := range []string{"present", "optional"} { // the writer is upgraded between passes
		if err := sampleTokenInventory(context.Background(), fakeGetter{obj: inv(e), status: 200}, reg, time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	out := metricsDump(t, reg)
	if n := strings.Count(out, `llz_credential_configured{cred="harbor-password"}`); n != 1 {
		t.Errorf("got %d configured series for one credential, want 1 — a reclassification stranded a stale label set:\n%s", n, out)
	}
	if n := strings.Count(out, `llz_credential_presence_ok{cred="harbor-password"}`); n != 1 {
		t.Errorf("got %d presence_ok series for one credential, want 1:\n%s", n, out)
	}
	// And the surviving verdict is the CURRENT one: optional is configreadiness.Satisfied by an
	// absent credential, so nothing alerts.
	if !strings.Contains(out, `llz_credential_presence_ok{cred="harbor-password"} 1`) {
		t.Errorf("the latest classification must win:\n%s", out)
	}
}

func TestPresenceMatchesExpectation(t *testing.T) {
	for _, tc := range []struct {
		expect  string
		present bool
		want    bool
	}{
		{tokeninv.CredExpectPresent, true, true},
		{tokeninv.CredExpectPresent, false, false},
		{tokeninv.CredExpectAbsent, false, true},  // the healthy root-token state
		{tokeninv.CredExpectAbsent, true, false},  // parked after a break-glass
		{tokeninv.CredExpectOptional, true, true}, // the Harbor pair, either way
		{tokeninv.CredExpectOptional, false, true},
		{"", true, true}, // an older writer sent no expect: treat as present
		{"", false, false},
	} {
		if got := presenceMatchesExpectation(tc.expect, tc.present); got != tc.want {
			t.Errorf("presenceMatchesExpectation(%q, %v) = %v, want %v", tc.expect, tc.present, got, tc.want)
		}
	}
}

// Retiring a credential must retire its series. This is the remedy
// LLZCredentialUnconfigured's own description recommends — "drop it from
// ghSecretTargets if this instance genuinely does not use it" — and under
// SetGauge, taking that advice froze the last sample at presence_ok=0, so the
// alert fired forever until someone restarted the reconciler. The documented fix
// for a false page made the page permanent.
func TestRetiredCredentialLeavesTheMetrics(t *testing.T) {
	cm := func(body string) map[string]any {
		return map[string]any{"data": map[string]any{"inventory.json": body}}
	}
	const kept = `{"name":"TF_STATE_ACCESS_KEY","class":"on-demand","expect":"present","state":"ok","updated_at":"2026-05-01T00:00:00Z"}`
	before := `{"updated":1,"tokens":[],"secret_probe":"ok","secrets":[
	  {"name":"GONE_TOKEN","class":"static","expect":"present","state":"absent"},` + kept + `]}`
	after := `{"updated":2,"tokens":[],"secret_probe":"ok","secrets":[` + kept + `]}`

	reg := metrics.NewRegistry()
	for _, b := range []string{before, after} {
		if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm(b), status: 200}, reg, time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	out := metricsDump(t, reg)
	if strings.Contains(out, `cred="gone-token"`) {
		t.Errorf("a retired credential must leave no series behind:\n%s", out)
	}
	// …and the credential that stayed is untouched.
	if !strings.Contains(out, `llz_credential_presence_ok{cred="tf-state-access-key"} 1`) {
		t.Errorf("the surviving credential must still be published:\n%s", out)
	}
}

// The same rule with nothing left at all: an inventory whose probe could not
// authenticate carries no secrets, and the previous pass's verdicts must not
// stand in for measurements that were never taken. The funnel gauge is what says
// why the series went away.
func TestUnreadableInventoryClearsPresenceSeries(t *testing.T) {
	cm := func(body string) map[string]any {
		return map[string]any{"data": map[string]any{"inventory.json": body}}
	}
	reg := metrics.NewRegistry()
	ok := `{"updated":1,"tokens":[],"secret_probe":"ok","secrets":[
	  {"name":"OPENBAO_SEAL_KEY","class":"static","expect":"present","state":"absent"}]}`
	dark := `{"updated":2,"tokens":[],"secret_probe":"unavailable"}`
	for _, b := range []string{ok, dark} {
		if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm(b), status: 200}, reg, time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	out := metricsDump(t, reg)
	if strings.Contains(out, `cred="openbao-seal-key"`) {
		t.Errorf("a pass that measured nothing must not leave stale verdicts:\n%s", out)
	}
	if !strings.Contains(out, "llz_credential_secret_probe_ok 0") {
		t.Errorf("the funnel gauge must say why:\n%s", out)
	}
}
