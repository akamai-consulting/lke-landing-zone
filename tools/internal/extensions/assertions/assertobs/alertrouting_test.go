package assertobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

var (
	pdFQN    = clusterspec.AlertingReceiverFQN(clusterspec.AlertingConfigName, clusterspec.AlertingReceiverPagerDuty)
	slackFQN = clusterspec.AlertingReceiverFQN(clusterspec.AlertingSlackConfigName, clusterspec.AlertingReceiverSlack)
)

// cluster is what a fake cluster has deployed: which receivers are loaded, and
// how its routing tree behaves. Each knob is one way the real thing breaks.
type cluster struct {
	pdKey      string   // "" = loaded with no key; "-" = PagerDuty CR not loaded
	aplSlack   bool     // apl-core's alerts.receivers include slack
	llzSlack   bool     // LLZ's Slack CR loaded
	pdSev      []string // severities LLZ's PagerDuty route matches
	llzSlackSv []string // severities LLZ's Slack route matches
	nsScoped   bool     // the operator's DEFAULT matcher strategy: LLZ routes only match namespace=monitoring
	aplWarning bool     // apl-core (or a hand-made CR) ALSO posts warnings to Slack — a duplicate
}

// status is /api/v2/status as Alertmanager reports it once the operator has
// merged LLZ's CRs into apl-core's base config: CR receivers renamed
// <ns>/<cr>/<name>, secrets masked as "<secret>", apl-core's Slack receivers
// inheriting global slack_api_url_file.
func (c cluster) status(t *testing.T) []byte {
	t.Helper()
	var b strings.Builder
	if c.aplSlack {
		b.WriteString("global:\n  slack_api_url_file: /etc/alertmanager/secrets/alertmanager-credentials/slackUrl\n")
	}
	b.WriteString("route:\n  receiver: \"null\"\nreceivers:\n- name: \"null\"\n")
	if c.aplSlack {
		b.WriteString("- name: default\n  slack_configs:\n  - channel: '#x'\n- name: critical\n  slack_configs:\n  - channel: '#x'\n")
	}
	if c.pdKey != "-" {
		fmt.Fprintf(&b, "- name: %s\n  pagerduty_configs:\n  - routing_key: %q\n", pdFQN, c.pdKey)
	}
	if c.llzSlack {
		fmt.Fprintf(&b, "- name: %s\n  slack_configs:\n  - api_url: <secret>\n    channel: '#x'\n", slackFQN)
	}
	raw, err := json.Marshal(map[string]any{"config": map[string]any{"original": b.String()}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// route is the cluster's routing tree, close enough to Alertmanager's: LLZ's
// CRs first (each with the operator's forced continue), then apl-core's base.
func (c cluster) route(l map[string]string) []string {
	var out []string
	sev, ns := l["severity"], l["namespace"]
	inScope := !c.nsScoped || ns == clusterspec.AlertmanagerNamespace
	if c.pdKey != "-" && inScope && slices.Contains(c.pdSev, sev) {
		out = append(out, pdFQN)
	}
	if c.llzSlack && inScope && slices.Contains(c.llzSlackSv, sev) {
		out = append(out, slackFQN)
	}
	switch {
	case sev == "critical" && c.aplSlack:
		out = append(out, "critical")
	case sev == "warning" && c.aplWarning:
		out = append(out, "default")
	}
	if len(out) == 0 {
		out = []string{"null"}
	}
	return out
}

// healthy is the cluster spec.alerting=[pagerduty, slack] should produce.
func healthy() cluster {
	return cluster{pdKey: "<secret>", aplSlack: true, llzSlack: true, pdSev: []string{"critical"}, llzSlackSv: []string{"warning"}}
}

var (
	pdSpec   = clusterspec.Alerting{Receivers: []string{"pagerduty"}}
	bothSpec = clusterspec.Alerting{Receivers: []string{"pagerduty", "slack"}}
)

func pdOnly() cluster {
	return cluster{pdKey: "<secret>", pdSev: []string{"critical"}}
}

// ── the fake Alertmanager ────────────────────────────────────────────────────

// fakeAM is just enough of Alertmanager's v2 API (and /metrics) for the gate.
type fakeAM struct {
	status        []byte
	silenceState  string
	route         func(labels map[string]string) []string
	metrics       string
	calls         []string
	posted        []map[string]string
	silenceFails  bool
	silenceDelete bool
}

const amMetrics = `# HELP alertmanager_notifications_total The total number of attempted notifications.
# TYPE alertmanager_notifications_total counter
alertmanager_notifications_total{integration="pagerduty",receiver_name="monitoring/llz-alerting/pagerduty"} 4
alertmanager_notifications_total{integration="slack",receiver_name="critical"} 7
alertmanager_notifications_total{integration="webhook"} 99
# TYPE alertmanager_notifications_failed_total counter
alertmanager_notifications_failed_total{integration="pagerduty",reason="other",receiver_name="monitoring/llz-alerting/pagerduty"} 2
alertmanager_notifications_failed_total{integration="slack",reason="other",receiver_name="critical"} 0
`

func newAM(t *testing.T, c cluster) *fakeAM {
	return &fakeAM{status: c.status(t), silenceState: "active", route: c.route, metrics: amMetrics}
}

func (f *fakeAM) do(method, path string, body []byte) ([]byte, error) {
	f.calls = append(f.calls, method+" "+strings.SplitN(path, "?", 2)[0])
	switch {
	case method == "DELETE":
		f.silenceDelete = true
		return nil, nil
	case method == "GET" && path == "/api/v2/status":
		return f.status, nil
	case method == "GET" && path == "/metrics":
		return []byte(f.metrics), nil
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

type routingFakes struct {
	teams       map[string]*fakeAM // namespace → team Alertmanager
	listFails   bool
	slackSecret struct{ set, ok bool }
}

func installAlertRoutingFakes(t *testing.T, spec clusterspec.Alerting, specErr error, am *fakeAM, transportErr error) *routingFakes {
	t.Helper()
	oldW, oldL, oldN, oldS, oldID, oldSec, oldList := withAlertmanager, loadAlertingSpec, alertRoutingNow, alertRoutingSleep, newProbeID, secretKeySet, listAlertmanagers
	t.Cleanup(func() {
		withAlertmanager, loadAlertingSpec, alertRoutingNow, alertRoutingSleep, newProbeID, secretKeySet, listAlertmanagers =
			oldW, oldL, oldN, oldS, oldID, oldSec, oldList
	})
	rf := &routingFakes{teams: map[string]*fakeAM{}}
	rf.slackSecret.set, rf.slackSecret.ok = true, true
	loadAlertingSpec = func(string) (alertingTarget, error) {
		return alertingTarget{Env: "lab", Alerting: spec, Applies: true}, specErr
	}
	withAlertmanager = func(target string, fn func(do httpDo) error) error {
		if transportErr != nil {
			return transportErr
		}
		ns, _, _ := strings.Cut(target, "/")
		if team, ok := rf.teams[ns]; ok {
			return fn(team.do)
		}
		return fn(am.do)
	}
	secretKeySet = func(ns, name, key string) (bool, bool) {
		if ns != clusterspec.AlertmanagerNamespace || name != clusterspec.AplAlertmanagerCredentialsSecret || key != clusterspec.AplAlertmanagerCredentialsSlackKey {
			t.Errorf("the gate checked %s/%s.%s, not apl-core's mounted Slack Secret", ns, name, key)
		}
		return rf.slackSecret.set, rf.slackSecret.ok
	}
	listAlertmanagers = func() ([]string, bool) {
		if rf.listFails {
			return nil, false
		}
		refs := []string{"monitoring/po-alertmanager"}
		for ns := range rf.teams {
			refs = append(refs, ns+"/"+strings.TrimPrefix(ns, "team-")+"-po-alertmanager")
		}
		slices.Sort(refs)
		return refs, true
	}
	// A clock that advances on every read, so the settle loop terminates.
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	alertRoutingNow = func() time.Time { clock = clock.Add(time.Minute); return clock }
	alertRoutingSleep = func(time.Duration) {}
	newProbeID = func() string { return "run1" }
	return rf
}

func run() error { return runCIAssertAlertRouting("lab", "monitoring/x:9093", 0, 0) }

// ── receivers loaded ─────────────────────────────────────────────────────────

func TestParseLoadedConfigReadsTheOperatorShapedConfig(t *testing.T) {
	got, err := parseLoadedConfig(healthy().status(t))
	if err != nil {
		t.Fatal(err)
	}
	if pd := got.Receivers[pdFQN]; pd.PagerDutyConfigs != 1 || pd.PagerDutyKeysSet != 1 {
		t.Errorf("pagerduty receiver = %+v", pd)
	}
	// apl-core's Slack receivers carry no api_url; the global file is their URL.
	if c := got.Receivers["critical"]; c.SlackConfigs != 1 || c.SlackURLsSet != 1 {
		t.Errorf("apl-core's critical receiver = %+v — the global slack_api_url_file must count as its URL", c)
	}
	if problems := gradeLoadedReceivers(got, clusterspec.AlertingExpectedReceivers(bothSpec)); len(problems) != 0 {
		t.Errorf("a fully loaded config graded with problems: %v", problems)
	}
}

// Could-not-tell is an error, never an empty config that grades as "missing".
func TestParseLoadedConfigFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"not json":            "<html>",
		"no config":           `{"uptime":"x"}`,
		"empty original":      `{"config":{"original":""}}`,
		"unparseable yaml":    `{"config":{"original":"receivers: [unterminated"}}`,
		"no receivers at all": `{"config":{"original":"route:\n  receiver: x\n"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLoadedConfig([]byte(body)); err == nil {
				t.Error("parsed clean — a response this gate does not understand must not grade as anything")
			}
		})
	}
}

func TestGradeLoadedReceiversCatchesEachMissingLink(t *testing.T) {
	expected := clusterspec.AlertingExpectedReceivers(bothSpec)
	for name, tc := range map[string]struct {
		c    cluster
		want string
	}{
		"an empty routing key":                       {cluster{pdKey: "", aplSlack: true, llzSlack: true}, "no routing_key"},
		"the PagerDuty CR rejected":                  {cluster{pdKey: "-", aplSlack: true, llzSlack: true}, "not in Alertmanager's running config"},
		"the AplAlertSet never reached apl-core":     {cluster{pdKey: "<secret>", llzSlack: true}, "not in Alertmanager's running config"},
		"the Slack CR rejected (secret not present)": {cluster{pdKey: "<secret>", aplSlack: true}, clusterspec.AlertingSlackConfigName},
	} {
		t.Run(name, func(t *testing.T) {
			loaded, err := parseLoadedConfig(tc.c.status(t))
			if err != nil {
				t.Fatal(err)
			}
			p := gradeLoadedReceivers(loaded, expected)
			if len(p) == 0 || !strings.Contains(strings.Join(p, "; "), tc.want) {
				t.Errorf("graded %v, want a problem mentioning %q", p, tc.want)
			}
		})
	}
}

// THE STATUS API SHOWS THE PATH, NOT THE FILE. A Slack receiver whose
// alertmanager-credentials Secret is empty or missing looks identical to a
// working one in /api/v2/status, so the Secret itself is read — and an
// unreadable one is "could not tell", a failure.
func TestTheSlackSecretBehindTheGlobalFileIsChecked(t *testing.T) {
	for name, s := range map[string]struct{ set, ok bool }{"empty": {false, true}, "unreadable": {false, false}} {
		t.Run(name, func(t *testing.T) {
			am := newAM(t, healthy())
			rf := installAlertRoutingFakes(t, bothSpec, nil, am, nil)
			rf.slackSecret.set, rf.slackSecret.ok = s.set, s.ok
			if err := run(); err == nil {
				t.Fatal("passed with apl-core's Slack Secret " + name)
			}
			if len(am.posted) != 0 {
				t.Error("probed with the Slack credential unverified")
			}
		})
	}
}

// ── routing ──────────────────────────────────────────────────────────────────

func TestAlertRoutingPassesWhenAlertmanagerRoutesAsTheSpecSays(t *testing.T) {
	for name, tc := range map[string]struct {
		spec clusterspec.Alerting
		c    cluster
	}{
		"pagerduty":         {pdSpec, pdOnly()},
		"pagerduty + slack": {bothSpec, healthy()},
	} {
		t.Run(name, func(t *testing.T) {
			am := newAM(t, tc.c)
			installAlertRoutingFakes(t, tc.spec, nil, am, nil)
			if err := run(); err != nil {
				t.Fatalf("a correctly routing Alertmanager failed: %v", err)
			}
			// IT NEVER NOTIFIES: the silence is created and verified before any
			// probe, and never expired by the gate.
			silenceAt, verifyAt, postAt := -1, -1, -1
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
			if len(am.posted) != len(clusterspec.AlertRoutingProbes(tc.spec)) {
				t.Errorf("posted %d probes, want %d", len(am.posted), len(clusterspec.AlertRoutingProbes(tc.spec)))
			}
		})
	}
}

// Every way the routing tree goes wrong while every receiver is loaded and keyed.
func TestAlertRoutingFailsOnEachMisroute(t *testing.T) {
	for name, tc := range map[string]struct {
		spec clusterspec.Alerting
		c    func() cluster
	}{
		// THE REGRESSION A CONFIG READ CANNOT SEE: receiver loaded, key set, CR
		// accepted — and the route scoped to namespace=monitoring.
		"namespace-scoped route": {pdSpec, func() cluster { c := pdOnly(); c.nsScoped = true; return c }},
		// A pager that fires on everything is not "routing correctly".
		"pages on info": {pdSpec, func() cluster { c := pdOnly(); c.pdSev = []string{"critical", "warning", "info"}; return c }},
		// DUPLICATE: a second Slack path for warnings (apl-core or a hand-made
		// CR) — "reaches Slack" holds, and every warning posts twice.
		"two Slack paths for warning": {bothSpec, func() cluster { c := healthy(); c.aplWarning = true; return c }},
		// DUPLICATE the other way: LLZ's Slack route also carrying critical.
		"two Slack paths for critical": {bothSpec, func() cluster { c := healthy(); c.llzSlackSv = []string{"warning", "critical"}; return c }},
		// The spec has no Slack, but the Console turned apl-core's on: an
		// unspecified path the AplAlertSet [none] should have removed.
		"Slack behind a pagerduty-only spec": {pdSpec, func() cluster { c := pdOnly(); c.aplSlack = true; return c }},
		// Warnings reach nothing: LLZ's Slack route missing its severity.
		"warning reaches no Slack": {bothSpec, func() cluster { c := healthy(); c.llzSlackSv = nil; return c }},
	} {
		t.Run(name, func(t *testing.T) {
			am := newAM(t, tc.c())
			installAlertRoutingFakes(t, tc.spec, nil, am, nil)
			if err := run(); err == nil {
				t.Fatal("passed")
			}
		})
	}
}

// NO ACTIVE SILENCE, NO PROBES. Posting under a pending/expired silence is the
// one way this gate could page someone.
func TestAlertRoutingPostsNothingWithoutAnActiveSilence(t *testing.T) {
	for _, state := range []string{"pending", "expired", ""} {
		am := newAM(t, pdOnly())
		am.silenceState = state
		installAlertRoutingFakes(t, pdSpec, nil, am, nil)
		if err := run(); err == nil {
			t.Errorf("silence %q: the gate passed", state)
		}
		if len(am.posted) != 0 {
			t.Errorf("silence %q: %d probe alerts were posted anyway", state, len(am.posted))
		}
	}
	am := newAM(t, pdOnly())
	am.silenceFails = true
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	if err := run(); err == nil || len(am.posted) != 0 {
		t.Errorf("a refused silence: err=%v, posted=%d", err, len(am.posted))
	}
}

// RECEIVER NOT LOADED → fail after the settle window, and never probe.
func TestAlertRoutingFailsWhenTheReceiverNeverLoads(t *testing.T) {
	c := pdOnly()
	c.pdKey = "-"
	am := newAM(t, c)
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	if err := run(); err == nil {
		t.Fatal("a missing receiver passed")
	}
	if len(am.posted) != 0 {
		t.Error("probes were posted with the receiver not even loaded")
	}
}

func TestAlertRoutingFailsClosedOnTransportAndSpec(t *testing.T) {
	installAlertRoutingFakes(t, pdSpec, nil, &fakeAM{}, errors.New("port-forward refused"))
	if err := run(); err == nil {
		t.Error("an unreachable Alertmanager passed")
	}
	installAlertRoutingFakes(t, clusterspec.Alerting{}, errors.New("no landingzone.yaml"), &fakeAM{}, nil)
	if err := run(); err == nil {
		t.Error("an unreadable spec passed — that is 'could not tell', not 'nothing configured'")
	}
}

// NOT APPLICABLE is a pass, and it must not touch Alertmanager at all — for no
// receiver, an opt-out, and an env without observability alike.
func TestAlertRoutingIsNotApplicableWithoutAReceiverOrObservability(t *testing.T) {
	for name, tgt := range map[string]alertingTarget{
		"unset":            {Env: "lab", Applies: true},
		"none":             {Env: "lab", Applies: true, Alerting: clusterspec.Alerting{Receivers: []string{"none"}}},
		"no observability": {Env: "lab", Applies: false, Alerting: bothSpec},
	} {
		t.Run(name, func(t *testing.T) {
			am := &fakeAM{}
			installAlertRoutingFakes(t, tgt.Alerting, nil, am, nil)
			loadAlertingSpec = func(string) (alertingTarget, error) { return tgt, nil }
			if err := run(); err != nil {
				t.Errorf("%v", err)
			}
			if len(am.calls) != 0 {
				t.Errorf("the gate called Alertmanager (%v) with nothing to check", am.calls)
			}
		})
	}
}

// A probe Alertmanager never reports was not examined — not a pass. And a
// receiver the alert reached that the config does not declare cannot be
// classified; counting it as zero would launder a duplicate.
func TestGradeProbeRoutingFailsClosed(t *testing.T) {
	loaded, err := parseLoadedConfig(pdOnly().status(t))
	if err != nil {
		t.Fatal(err)
	}
	probes := clusterspec.AlertRoutingProbes(pdSpec)
	v, err := gradeProbeRouting([]byte(`[]`), probes, "run1", loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		if x.OK || x.Seen {
			t.Errorf("probe %q graded OK/seen from an empty response", x.Probe.Name)
		}
	}
	// Another run's alert with the same case number must not stand in for ours.
	other := `[{"labels":{"alertname":"LLZAlertRoutingProbe","llz_probe":"other","llz_probe_case":"0"},"receivers":[{"name":"` + pdFQN + `"}]}]`
	if v, _ = gradeProbeRouting([]byte(other), probes, "run1", loaded); v[0].Seen {
		t.Error("another run's probe alert was graded as this run's")
	}
	unknown := `[{"labels":{"alertname":"LLZAlertRoutingProbe","llz_probe":"run1","llz_probe_case":"0"},"receivers":[{"name":"someone-elses"}]}]`
	if _, err := gradeProbeRouting([]byte(unknown), probes, "run1", loaded); err == nil {
		t.Error("an unclassifiable receiver graded clean")
	}
	if _, err := gradeProbeRouting([]byte(`{"not":"a list"}`), probes, "run1", loaded); err == nil {
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
	ab, err := probeAlertsBody(clusterspec.AlertRoutingProbes(bothSpec), "run1", now)
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
// the renderer puts the AlertmanagerConfigs in another constant. The matcher
// exemption applies only if they are the same namespace.
func TestTheGateTalksToTheAlertmanagerTheCRLivesBeside(t *testing.T) {
	ns, _, ok := strings.Cut(defaultAlertmanagerSpec, "/")
	if !ok || ns != clusterspec.AlertmanagerNamespace {
		t.Errorf("assert-alert-routing probes the Alertmanager in %q, the AlertmanagerConfigs are rendered into %q",
			ns, clusterspec.AlertmanagerNamespace)
	}
}

// THE STRATEGY CAN TRAIL THE CR on a fresh cluster (it arrives through
// apl-operator, the CR through Argo), so a misroute inside the settle window is
// retried — with a FRESH probe id, so the first run's alerts cannot vouch for
// the second.
func TestAlertRoutingRetriesAMisrouteWithAFreshProbe(t *testing.T) {
	scoped, good := pdOnly(), pdOnly()
	scoped.nsScoped = true
	am := newAM(t, good)
	batches := 0
	am.route = func(l map[string]string) []string {
		if l[probeIDLabel] == "run1" {
			return scoped.route(l)
		}
		return good.route(l)
	}
	installAlertRoutingFakes(t, pdSpec, nil, am, nil)
	ids := []string{"run1", "run2", "run3"}
	newProbeID = func() string { id := ids[batches]; batches++; return id }
	if err := runCIAssertAlertRouting("lab", "monitoring/x:9093", time.Hour, 0); err != nil {
		t.Fatalf("a route that healed on the second attempt still failed: %v", err)
	}
	if batches != 2 {
		t.Errorf("probed %d times, want 2 (one misroute, one pass)", batches)
	}
}

// ── team Alertmanagers ───────────────────────────────────────────────────────

// A TEAM ALERTMANAGER THAT MERGED LLZ's CRs PAGES A SECOND TIME. apl-core's team
// Alertmanagers select every AlertmanagerConfig and receive every platform
// alert; LLZ cannot scope them, so the gate reads each one and fails.
func TestATeamAlertmanagerCarryingLLZReceiversFails(t *testing.T) {
	am := newAM(t, healthy())
	rf := installAlertRoutingFakes(t, bothSpec, nil, am, nil)
	team := newAM(t, cluster{pdKey: "<secret>"}) // merged LLZ's PagerDuty CR
	rf.teams["team-demo"] = team
	if err := run(); err == nil {
		t.Fatal("a team Alertmanager carrying LLZ's PagerDuty receiver passed")
	}
	if len(team.posted) != 0 {
		t.Error("the gate posted probes to a team Alertmanager — only its config is read")
	}
}

func TestTeamAlertmanagersWithoutLLZReceiversPass(t *testing.T) {
	am := newAM(t, healthy())
	rf := installAlertRoutingFakes(t, bothSpec, nil, am, nil)
	rf.teams["team-demo"] = newAM(t, cluster{pdKey: "-", aplSlack: true}) // the team's own receivers only
	if err := run(); err != nil {
		t.Fatalf("a clean team Alertmanager failed the gate: %v", err)
	}
}

// Could-not-list is "could not tell" — not "no team Alertmanagers".
func TestAnUnlistableAlertmanagerSetFails(t *testing.T) {
	am := newAM(t, healthy())
	rf := installAlertRoutingFakes(t, bothSpec, nil, am, nil)
	rf.listFails = true
	if err := run(); err == nil {
		t.Fatal("an unreadable Alertmanager list passed as 'no team Alertmanagers'")
	}
}

// ── delivery (report only) ───────────────────────────────────────────────────

func TestParseNotificationStats(t *testing.T) {
	got, err := parseNotificationStats([]byte(amMetrics))
	if err != nil {
		t.Fatal(err)
	}
	want := []deliveryStat{{"pagerduty", 4, 2}, {"slack", 7, 0}}
	if !slices.Equal(got, want) {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
	if _, err := parseNotificationStats([]byte("# nothing here\nup 1\n")); err == nil {
		t.Error("a /metrics without Alertmanager's notification family parsed clean")
	}
}

// REPORT ONLY: failed notifications are reported, never a failure — the release
// e2e points every receiver at a `.invalid` endpoint on purpose.
func TestFailedNotificationsDoNotFailTheGate(t *testing.T) {
	am := newAM(t, healthy())
	installAlertRoutingFakes(t, bothSpec, nil, am, nil)
	if err := run(); err != nil {
		t.Fatalf("failed notifications (report-only) failed the gate: %v", err)
	}
	if !slices.Contains(am.calls, "GET /metrics") {
		t.Error("the delivery counters were never read")
	}
}
