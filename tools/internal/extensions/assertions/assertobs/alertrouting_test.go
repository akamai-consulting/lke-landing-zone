package assertobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// operatorConfig is what Alertmanager reports in /api/v2/status config.original
// once prometheus-operator has merged spec.alerting's CR: receivers renamed
// <ns>/<cr>/<name>, secrets masked as "<secret>".
func operatorConfig(t *testing.T, pdKey, slackURL string, omit ...string) []byte {
	t.Helper()
	skip := map[string]bool{}
	for _, o := range omit {
		skip[o] = true
	}
	var b strings.Builder
	b.WriteString("route:\n  receiver: \"null\"\nreceivers:\n- name: \"null\"\n")
	if !skip["pagerduty"] {
		fmt.Fprintf(&b, "- name: %s\n  pagerduty_configs:\n  - routing_key: %q\n",
			clusterspec.AlertingReceiverFQN(clusterspec.AlertingReceiverPagerDuty), pdKey)
	}
	for _, r := range []string{clusterspec.AlertingReceiverSlackCritical, clusterspec.AlertingReceiverSlackDefault} {
		if !skip["slack"] {
			fmt.Fprintf(&b, "- name: %s\n  slack_configs:\n  - api_url: %q\n    channel: '#x'\n",
				clusterspec.AlertingReceiverFQN(r), slackURL)
		}
	}
	raw, err := json.Marshal(map[string]any{"config": map[string]any{"original": b.String()}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLoadedReceiversReadsTheOperatorShapedConfig(t *testing.T) {
	got, err := loadedReceivers(operatorConfig(t, "<secret>", "<secret>"))
	if err != nil {
		t.Fatal(err)
	}
	pd := got[clusterspec.AlertingReceiverFQN(clusterspec.AlertingReceiverPagerDuty)]
	if pd.PagerDutyConfigs != 1 || pd.PagerDutyKeysSet != 1 {
		t.Errorf("pagerduty receiver = %+v", pd)
	}
	if problems := gradeLoadedReceivers(got, clusterspec.AlertingExpectedReceivers(clusterspec.Alerting{Receivers: []string{"pagerduty", "slack"}})); len(problems) != 0 {
		t.Errorf("a fully loaded config graded with problems: %v", problems)
	}
}

// Could-not-tell is an error, never an empty map that grades as "missing".
func TestLoadedReceiversFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"not json":            "<html>",
		"no config":           `{"uptime":"x"}`,
		"empty original":      `{"config":{"original":""}}`,
		"unparseable yaml":    `{"config":{"original":"receivers: [unterminated"}}`,
		"no receivers at all": `{"config":{"original":"route:\n  receiver: x\n"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadedReceivers([]byte(body)); err == nil {
				t.Error("parsed clean — a response this gate does not understand must not grade as anything")
			}
		})
	}
}

// AN UNSEEDED KEY. If the operator inlined an empty key (or a future operator
// renders the receiver without one), the receiver is present and pages nothing.
func TestGradeLoadedReceiversCatchesAMissingKeyAndAMissingReceiver(t *testing.T) {
	expected := clusterspec.AlertingExpectedReceivers(clusterspec.Alerting{Receivers: []string{"pagerduty", "slack"}})

	noKey, err := loadedReceivers(operatorConfig(t, "", "<secret>"))
	if err != nil {
		t.Fatal(err)
	}
	if p := gradeLoadedReceivers(noKey, expected); len(p) != 1 || !strings.Contains(p[0], "no routing_key") {
		t.Errorf("an empty routing key graded %v", p)
	}

	noPD, err := loadedReceivers(operatorConfig(t, "<secret>", "<secret>", "pagerduty"))
	if err != nil {
		t.Fatal(err)
	}
	if p := gradeLoadedReceivers(noPD, expected); len(p) != 1 || !strings.Contains(p[0], "not in Alertmanager's running config") {
		t.Errorf("a missing pagerduty receiver graded %v", p)
	}
}

// ── the fake Alertmanager ────────────────────────────────────────────────────

// fakeAM is just enough of Alertmanager's v2 API for the gate. route decides
// receivers for a label set — the stand-in for Alertmanager's routing tree.
type fakeAM struct {
	status        []byte
	silenceState  string
	route         func(labels map[string]string) []string
	calls         []string
	posted        []map[string]string
	silenceFails  bool
	silenceDelete bool
}

func (f *fakeAM) do(method, path string, body []byte) ([]byte, error) {
	f.calls = append(f.calls, method+" "+strings.SplitN(path, "?", 2)[0])
	switch {
	case method == "DELETE":
		f.silenceDelete = true
		return nil, nil
	case method == "GET" && path == "/api/v2/status":
		return f.status, nil
	case method == "POST" && path == "/api/v2/silences":
		if f.silenceFails {
			return nil, errors.New("HTTP 400")
		}
		return []byte(`{"silenceID":"sil-1"}`), nil
	case method == "GET" && strings.HasPrefix(path, "/api/v2/silence/"):
		return []byte(fmt.Sprintf(`{"id":"sil-1","status":{"state":%q}}`, f.silenceState)), nil
	case method == "POST" && path == "/api/v2/alerts":
		var alerts []struct {
			Labels map[string]string `json:"labels"`
		}
		if err := json.Unmarshal(body, &alerts); err != nil {
			return nil, err
		}
		for _, a := range alerts {
			f.posted = append(f.posted, a.Labels)
		}
		return nil, nil
	case method == "GET" && strings.HasPrefix(path, "/api/v2/alerts"):
		q, _ := url.ParseQuery(strings.SplitN(path, "?", 2)[1])
		want := q.Get("filter")
		var out []map[string]any
		for _, l := range f.posted {
			if fmt.Sprintf(`%s="%s"`, probeIDLabel, l[probeIDLabel]) != want {
				continue
			}
			var recv []map[string]string
			for _, r := range f.route(l) {
				recv = append(recv, map[string]string{"name": r})
			}
			out = append(out, map[string]any{
				"labels": l, "receivers": recv,
				"status": map[string]any{"silencedBy": []string{"sil-1"}},
			})
		}
		return json.Marshal(out)
	}
	return nil, fmt.Errorf("fake: unexpected %s %s", method, path)
}

// correctRoute is Alertmanager with the matcher exemption in force.
func correctRoute(sev []string) func(map[string]string) []string {
	return func(l map[string]string) []string {
		out := []string{"null"}
		for _, s := range sev {
			if l["severity"] == s {
				out = append(out, clusterspec.AlertingReceiverFQN(clusterspec.AlertingReceiverPagerDuty))
			}
		}
		return out
	}
}

// namespaceScopedRoute is the operator's DEFAULT strategy: the CR's route only
// matches alerts labelled with its own namespace.
func namespaceScopedRoute(l map[string]string) []string {
	if l["namespace"] == clusterspec.AlertmanagerNamespace && l["severity"] == "critical" {
		return []string{clusterspec.AlertingReceiverFQN(clusterspec.AlertingReceiverPagerDuty)}
	}
	return []string{"null"}
}

func installAlertRoutingFakes(t *testing.T, spec clusterspec.Alerting, specErr error, am *fakeAM, transportErr error) {
	t.Helper()
	oldW, oldL, oldN, oldS, oldID := withAlertmanager, loadAlertingSpec, alertRoutingNow, alertRoutingSleep, newProbeID
	t.Cleanup(func() {
		withAlertmanager, loadAlertingSpec, alertRoutingNow, alertRoutingSleep, newProbeID = oldW, oldL, oldN, oldS, oldID
	})
	loadAlertingSpec = func() (clusterspec.Alerting, error) { return spec, specErr }
	withAlertmanager = func(_ string, fn func(do httpDo) error) error {
		if transportErr != nil {
			return transportErr
		}
		return fn(am.do)
	}
	// A clock that advances on every read, so the settle loop terminates.
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	alertRoutingNow = func() time.Time { clock = clock.Add(time.Minute); return clock }
	alertRoutingSleep = func(time.Duration) {}
	newProbeID = func() string { return "run1" }
}

var pdSpec = clusterspec.Alerting{Receivers: []string{"pagerduty"}}

func TestAlertRoutingPassesWhenAlertmanagerRoutesAsTheSpecSays(t *testing.T) {
	am := &fakeAM{status: operatorConfig(t, "<secret>", "<secret>", "slack"), silenceState: "active", route: correctRoute([]string{"critical"})}
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err != nil {
		t.Fatalf("a correctly routing Alertmanager failed: %v", err)
	}
	// IT NEVER PAGES: the silence is created and verified before any probe, and
	// never expired by the gate.
	var silenceAt, verifyAt, postAt = -1, -1, -1
	for i, c := range am.calls {
		switch c {
		case "POST /api/v2/silences":
			silenceAt = i
		case "GET /api/v2/silence/sil-1":
			verifyAt = i
		case "POST /api/v2/alerts":
			postAt = i
		}
	}
	if silenceAt < 0 || verifyAt < silenceAt || postAt < verifyAt {
		t.Errorf("call order %v — the silence must be created, then verified, then the probes posted", am.calls)
	}
	if am.silenceDelete {
		t.Error("the gate expired its silence — a probe still active would then page")
	}
	if len(am.posted) != len(clusterspec.AlertRoutingProbes(pdSpec)) {
		t.Errorf("posted %d probes, want %d", len(am.posted), len(clusterspec.AlertRoutingProbes(pdSpec)))
	}
}

// THE REGRESSION A CONFIG READ CANNOT SEE: receiver loaded, key set, CR
// accepted — and the route scoped to namespace=monitoring by the operator's
// default strategy. Only the probe finds it.
func TestAlertRoutingFailsOnANamespaceScopedRoute(t *testing.T) {
	am := &fakeAM{status: operatorConfig(t, "<secret>", "", "slack"), silenceState: "active", route: namespaceScopedRoute}
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err == nil {
		t.Fatal("a route that only pages for namespace=monitoring passed")
	}
}

// …and the inverse: a pager that fires on everything is not "routing correctly".
func TestAlertRoutingFailsWhenEverythingPages(t *testing.T) {
	everything := func(map[string]string) []string {
		return []string{clusterspec.AlertingReceiverFQN(clusterspec.AlertingReceiverPagerDuty)}
	}
	am := &fakeAM{status: operatorConfig(t, "<secret>", "", "slack"), silenceState: "active", route: everything}
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err == nil {
		t.Fatal("a route that pages on severity=info passed")
	}
}

// NO ACTIVE SILENCE, NO PROBES. Posting under a pending/expired silence is the
// one way this gate could page someone.
func TestAlertRoutingPostsNothingWithoutAnActiveSilence(t *testing.T) {
	for _, state := range []string{"pending", "expired", ""} {
		am := &fakeAM{status: operatorConfig(t, "<secret>", "", "slack"), silenceState: state, route: correctRoute([]string{"critical"})}
		installAlertRoutingFakes(t, pdSpec, nil, am, nil)
		if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err == nil {
			t.Errorf("silence %q: the gate passed", state)
		}
		if len(am.posted) != 0 {
			t.Errorf("silence %q: %d probe alerts were posted anyway", state, len(am.posted))
		}
	}
	am := &fakeAM{status: operatorConfig(t, "<secret>", "", "slack"), silenceFails: true, route: correctRoute([]string{"critical"})}
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err == nil || len(am.posted) != 0 {
		t.Errorf("a refused silence: err=%v, posted=%d", err, len(am.posted))
	}
}

// RECEIVER NOT LOADED → fail after the settle window, naming what IS loaded and
// what to check, and never probe (routing by a config that lacks the receiver
// would only add a second, less useful failure).
func TestAlertRoutingFailsWhenTheReceiverNeverLoads(t *testing.T) {
	am := &fakeAM{status: operatorConfig(t, "<secret>", "", "pagerduty", "slack"), silenceState: "active", route: correctRoute(nil)}
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err == nil {
		t.Fatal("a missing receiver passed")
	}
	if len(am.posted) != 0 {
		t.Error("probes were posted with the receiver not even loaded")
	}
}

func TestAlertRoutingFailsClosedOnTransportAndSpec(t *testing.T) {
	installAlertRoutingFakes(t, pdSpec, nil, &fakeAM{}, errors.New("port-forward refused"))
	if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err == nil {
		t.Error("an unreachable Alertmanager passed")
	}
	installAlertRoutingFakes(t, clusterspec.Alerting{}, errors.New("no landingzone.yaml"), &fakeAM{}, nil)
	if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err == nil {
		t.Error("an unreadable spec passed — that is 'could not tell', not 'nothing configured'")
	}
}

// NOT APPLICABLE is a pass, and it must not touch Alertmanager at all.
func TestAlertRoutingIsNotApplicableWithoutAReceiver(t *testing.T) {
	for _, a := range []clusterspec.Alerting{{}, {Receivers: []string{"none"}}} {
		am := &fakeAM{}
		installAlertRoutingFakes(t, a, nil, am, nil)
		if err := runCIAssertAlertRouting("monitoring/x:9093", 0, 0); err != nil {
			t.Errorf("receivers %v: %v", a.Receivers, err)
		}
		if len(am.calls) != 0 {
			t.Errorf("receivers %v: the gate called Alertmanager (%v) with nothing to check", a.Receivers, am.calls)
		}
	}
}

// A probe Alertmanager never reports was not examined — not a pass.
func TestGradeProbeRoutingFailsAnUnseenProbe(t *testing.T) {
	probes := clusterspec.AlertRoutingProbes(pdSpec)
	v, err := gradeProbeRouting([]byte(`[]`), probes, "run1")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		if x.OK || x.Seen {
			t.Errorf("probe %q graded OK/seen from an empty response", x.Probe.Name)
		}
	}
	// Another run's alert with the same case number must not stand in for ours.
	other := `[{"labels":{"alertname":"LLZAlertRoutingProbe","llz_probe":"other","llz_probe_case":"0"},"receivers":[{"name":"` +
		clusterspec.AlertingReceiverFQN(clusterspec.AlertingReceiverPagerDuty) + `"}]}]`
	v, err = gradeProbeRouting([]byte(other), probes, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if v[0].Seen {
		t.Error("another run's probe alert was graded as this run's")
	}
	if _, err := gradeProbeRouting([]byte(`{"not":"a list"}`), probes, "run1"); err == nil {
		t.Error("a malformed /api/v2/alerts graded clean")
	}
}

// THE SILENCE COVERS EXACTLY THIS RUN'S PROBES, and outlives them.
func TestTheSilenceMatchesTheProbesAndOutlivesThem(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sb, err := silenceBody("run1", now)
	if err != nil {
		t.Fatal(err)
	}
	var sil struct {
		Matchers []struct {
			Name, Value      string
			IsRegex, IsEqual bool
		}
		StartsAt, EndsAt time.Time
	}
	if err := json.Unmarshal(sb, &sil); err != nil {
		t.Fatal(err)
	}
	ab, err := probeAlertsBody(clusterspec.AlertRoutingProbes(pdSpec), "run1", now)
	if err != nil {
		t.Fatal(err)
	}
	var alerts []struct {
		Labels   map[string]string
		EndsAt   time.Time
		StartsAt time.Time
	}
	if err := json.Unmarshal(ab, &alerts); err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		for _, m := range sil.Matchers {
			if m.IsRegex || !m.IsEqual || a.Labels[m.Name] != m.Value {
				t.Errorf("probe %v is not matched by silence matcher %+v — it could page", a.Labels, m)
			}
		}
		if !sil.StartsAt.Before(a.StartsAt) || !sil.EndsAt.After(a.EndsAt.Add(10*time.Minute)) {
			t.Errorf("the silence [%s, %s] does not comfortably cover the probe [%s, %s]",
				sil.StartsAt, sil.EndsAt, a.StartsAt, a.EndsAt)
		}
	}
}

// SPLIT CONTRACT: the gate port-forwards to Alertmanager in one namespace, and
// the renderer puts the AlertmanagerConfig in another constant. The matcher
// exemption applies only if they are the same namespace.
func TestTheGateTalksToTheAlertmanagerTheCRLivesBeside(t *testing.T) {
	ns, _, ok := strings.Cut(defaultAlertmanagerSpec, "/")
	if !ok || ns != clusterspec.AlertmanagerNamespace {
		t.Errorf("assert-alert-routing probes the Alertmanager in %q, the AlertmanagerConfig is rendered into %q",
			ns, clusterspec.AlertmanagerNamespace)
	}
}

// THE STRATEGY CAN TRAIL THE CR on a fresh cluster (it arrives through
// apl-operator, the CR through Argo), so a misroute inside the settle window is
// retried — with a FRESH probe id, so the first run's alerts cannot vouch for
// the second.
func TestAlertRoutingRetriesAMisrouteWithAFreshProbe(t *testing.T) {
	am := &fakeAM{status: operatorConfig(t, "<secret>", "", "slack"), silenceState: "active"}
	batches := 0
	am.route = func(l map[string]string) []string {
		if l[probeIDLabel] == "run1" {
			return namespaceScopedRoute(l)
		}
		return correctRoute([]string{"critical"})(l)
	}
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	ids := []string{"run1", "run2", "run3"}
	newProbeID = func() string { id := ids[batches]; batches++; return id }
	if err := runCIAssertAlertRouting("monitoring/x:9093", time.Hour, 0); err != nil {
		t.Fatalf("a route that healed on the second attempt still failed: %v", err)
	}
	if batches != 2 {
		t.Errorf("probed %d times, want 2 (one misroute, one pass)", batches)
	}
}
