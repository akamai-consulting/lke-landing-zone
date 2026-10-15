package clusterspec

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"

	yaml "gopkg.in/yaml.v3"
)

// renderedAlertingDocs decodes RenderAlertingManifest's output into its
// documents, keyed by kind/name. A test that string-matched the YAML would pass
// on a manifest kubectl could not even parse.
func renderedAlertingDocs(t *testing.T, a Alerting) map[string]map[string]any {
	t.Helper()
	body := RenderAlertingManifest(a)
	if body == "" {
		t.Fatalf("receivers %v rendered nothing", a.Receivers)
	}
	out := map[string]map[string]any{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(body)))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("rendered alerting manifest is not valid YAML: %v\n%s", err, body)
		}
		key := doc["kind"].(string) + "/" + dig(t, doc, "metadata", "name").(string)
		if _, dup := out[key]; dup {
			t.Fatalf("two %s documents rendered", key)
		}
		out[key] = doc
	}
	return out
}

func dig(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("at %q: %T is not a mapping", p, v)
		}
		v = m[p]
	}
	return v
}

func asList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%T is not a list", v)
	}
	return l
}

func alertingFixture() Alerting {
	return Alerting{Receivers: []string{"pagerduty", "slack"}}
}

const (
	pdCR    = "AlertmanagerConfig/" + AlertingConfigName
	slackCR = "AlertmanagerConfig/" + AlertingSlackConfigName
)

// ROUTES NOWHERE → RENDERS NOTHING in the observability app. The kustomization
// only lists the file when there is one, so an instance without alerting sees no
// drift. The AplAlertSet is different: an explicit [none] is still rendered, so
// apl-core's own receivers are pinned to [none] rather than left to whatever the
// Console last set.
func TestAlertingRendersNothingWhenItRoutesNowhere(t *testing.T) {
	for _, a := range []Alerting{{}, {Receivers: []string{"none"}}} {
		if got := RenderAlertingManifest(a); got != "" {
			t.Errorf("receivers %v rendered a manifest:\n%s", a.Receivers, got)
		}
		if a.Active() {
			t.Errorf("receivers %v reported Active", a.Receivers)
		}
		if p := AlertRoutingProbes(a); len(p) != 0 {
			t.Errorf("receivers %v produced %d routing probes — the gate would test a route nobody asked for", a.Receivers, len(p))
		}
	}
	if got := RenderAlertSetOverlay(Alerting{}); got != "" {
		t.Errorf("an undeclared spec.alerting rendered an AplAlertSet — LLZ would take apl-core's alert settings over:\n%s", got)
	}
	if got := RenderAlertSetOverlay(Alerting{Receivers: []string{"none"}}); !strings.Contains(got, "- none") {
		t.Errorf("receivers [none] must pin apl-core's receivers to [none]:\n%s", got)
	}
}

// THE CR MUST LIVE WHERE THE MATCHER EXEMPTION APPLIES. The apl-overlay sets
// OnNamespaceExceptForAlertmanagerNamespace, which exempts only CRs in
// Alertmanager's own namespace from the namespace="<cr ns>" matcher. Move a CR
// (or change the strategy) and it silently routes only alerts from one
// namespace — so both halves are read from what is actually rendered.
func TestAlertingCRsLiveWhereTheMatcherExemptionApplies(t *testing.T) {
	docs := renderedAlertingDocs(t, alertingFixture())
	for _, key := range []string{pdCR, slackCR} {
		amc := docs[key]
		if amc == nil {
			t.Fatalf("no %s rendered", key)
		}
		if ns := dig(t, amc, "metadata", "namespace"); ns != AlertmanagerNamespace {
			t.Errorf("%s is in %v, not Alertmanager's namespace %q — the operator would add "+
				"namespace=%v to its route and it would notify only for that namespace", key, ns, AlertmanagerNamespace, ns)
		}
	}
	rv, ok := AplAppRawValues()["alertmanager"]
	if !ok {
		t.Fatal("the apl-overlay asserts nothing on apps.alertmanager — the CRs' routes would carry a namespace matcher")
	}
	got, ok := RawValue(rv, "alertmanagerSpec", "alertmanagerConfigMatcherStrategy", "type")
	if !ok || got != "OnNamespaceExceptForAlertmanagerNamespace" {
		t.Errorf("apps.alertmanager matcher strategy = %v, want OnNamespaceExceptForAlertmanagerNamespace", got)
	}
	// And the probes must come from somewhere ELSE, or they would pass on the
	// regression: under OnNamespace a probe labelled `monitoring` still routes.
	for _, p := range AlertRoutingProbes(alertingFixture()) {
		if p.Labels["namespace"] == AlertmanagerNamespace {
			t.Errorf("probe %q is labelled namespace=%s — it cannot tell the exemption from its absence", p.Name, AlertmanagerNamespace)
		}
	}
}

// EVERY ROUTE NAMES A RECEIVER ITS CR DECLARES, and every Secret a receiver
// references is one something writes. The operator rejects the whole CR on
// either mistake, which from the pager's side is indistinguishable from not
// having configured one.
func TestAlertingRoutesAndSecretsAreInternallyConsistent(t *testing.T) {
	for _, recv := range [][]string{{"pagerduty"}, {"slack"}, {"pagerduty", "slack"}} {
		t.Run(strings.Join(recv, "+"), func(t *testing.T) {
			a := Alerting{Receivers: recv}
			docs := renderedAlertingDocs(t, a)
			// What each ExternalSecret writes, as namespace/secret → keys.
			written := map[string]map[string]bool{}
			for key, d := range docs {
				if !strings.HasPrefix(key, "ExternalSecret/") {
					continue
				}
				target := dig(t, d, "metadata", "namespace").(string) + "/" + dig(t, d, "spec", "target", "name").(string)
				written[target] = map[string]bool{}
				for _, e := range asList(t, dig(t, d, "spec", "data")) {
					written[target][dig(t, e, "secretKey").(string)] = true
				}
			}
			declaredFQN := map[string]bool{}
			for key, amc := range docs {
				if !strings.HasPrefix(key, "AlertmanagerConfig/") {
					continue
				}
				cr := dig(t, amc, "metadata", "name").(string)
				declared := map[string]bool{}
				for _, r := range asList(t, dig(t, amc, "spec", "receivers")) {
					name := dig(t, r, "name").(string)
					declared[name] = true
					declaredFQN[AlertingReceiverFQN(cr, name)] = true
					for _, field := range []struct{ list, ref string }{{"pagerdutyConfigs", "routingKey"}, {"slackConfigs", "apiURL"}} {
						cfgs, _ := r.(map[string]any)[field.list].([]any)
						for _, c := range cfgs {
							sel := dig(t, c, field.ref)
							secret := AlertmanagerNamespace + "/" + dig(t, sel, "name").(string)
							k := dig(t, sel, "key").(string)
							if secret == AlertmanagerNamespace+"/"+AplAlertmanagerCredentialsSecret {
								// apl-core's own Secret, fed by OUR alerts-secrets.
								if k != AplAlertmanagerCredentialsSlackKey {
									t.Errorf("receiver %s reads %s key %q; apl-core writes %q", name, secret, k, AplAlertmanagerCredentialsSlackKey)
								}
								if !written[AplSecretsNamespace+"/"+AplAlertsSecretName][aplAlertsSlackProperty] {
									t.Errorf("receiver %s reads apl-core's Slack Secret, but nothing writes %s/%s.%s for apl-core to copy",
										name, AplSecretsNamespace, AplAlertsSecretName, aplAlertsSlackProperty)
								}
								continue
							}
							if !written[secret][k] {
								t.Errorf("receiver %s needs Secret %s key %q, which no ExternalSecret writes", name, secret, k)
							}
						}
					}
				}
				route := dig(t, amc, "spec", "route")
				if !declared[dig(t, route, "receiver").(string)] {
					t.Errorf("%s: the top-level route's receiver %v is not declared", cr, dig(t, route, "receiver"))
				}
				for _, r := range asList(t, dig(t, route, "routes")) {
					if n := dig(t, r, "receiver").(string); !declared[n] {
						t.Errorf("%s: a route sends to %q, which no receiver of that CR declares", cr, n)
					}
				}
			}
			// The gate looks for the expected receivers by their loaded names, so
			// each LLZ one must be a receiver a rendered CR actually declares.
			for _, e := range AlertingExpectedReceivers(a) {
				if IsLLZAlertingReceiver(e.Name) && !declaredFQN[e.Name] {
					t.Errorf("the gate expects %s, but no rendered CR declares it", e.Name)
				}
			}
			for _, p := range AlertRoutingProbes(a) {
				for _, r := range p.MustReach {
					if IsLLZAlertingReceiver(r) && !declaredFQN[r] {
						t.Errorf("probe %q must reach %s, which no rendered CR declares", p.Name, r)
					}
				}
			}
		})
	}
}

// THE SLACK CR IS NOT THE PAGERDUTY CR. The Slack one reads apl-core's
// alertmanager-credentials, which only exists once apl-core applied the
// AplAlertSet; the operator rejects a CR whose Secret is absent, and a
// rejected CR drops every route in it. Sharing a CR would let a Slack secret
// problem take the pager down.
func TestPagerDutyAndSlackAreSeparateCRs(t *testing.T) {
	docs := renderedAlertingDocs(t, alertingFixture())
	for _, r := range asList(t, dig(t, docs[pdCR], "spec", "receivers")) {
		if _, ok := r.(map[string]any)["slackConfigs"]; ok {
			t.Errorf("the PagerDuty CR carries a Slack receiver — a missing Slack secret would get it rejected")
		}
	}
}

// A NOTIFIER THAT FIRES FOREVER IS AN OUTAGE TOO. Watchdog is always firing by
// design; our routes sit above apl-core's (which sends it to null), so it is
// our exclusion or nobody's — in EVERY CR.
func TestAlwaysFiringAlertsAreExcludedFromEveryCR(t *testing.T) {
	docs := renderedAlertingDocs(t, alertingFixture())
	for _, key := range []string{pdCR, slackCR} {
		found := false
		for _, m := range asList(t, dig(t, docs[key], "spec", "route", "matchers")) {
			if dig(t, m, "name") == "alertname" && dig(t, m, "matchType") == "!~" {
				v := dig(t, m, "value").(string)
				found = strings.Contains(v, "Watchdog") && strings.Contains(v, "InfoInhibitor")
			}
		}
		if !found {
			t.Errorf("%s: the top-level route does not exclude Watchdog/InfoInhibitor", key)
		}
	}
}

// PAGERDUTY PAGES ON THE DECLARED SEVERITIES AND NOTHING WIDER. (Slack for the
// same severity is another CR, or apl-core's route; the operator's forced
// top-level continue: true is what lets both run.)
func TestPagerDutyRouteMatchesTheDeclaredSeverities(t *testing.T) {
	cases := []struct {
		sev  []string
		want string
	}{
		{nil, "critical"},
		{[]string{"critical", "page"}, "critical|page"},
	}
	for _, tc := range cases {
		a := Alerting{Receivers: []string{"pagerduty"}, PagerDuty: AlertingPagerDuty{Severities: tc.sev}}
		routes := asList(t, dig(t, renderedAlertingDocs(t, a)[pdCR], "spec", "route", "routes"))
		if len(routes) != 1 || dig(t, routes[0], "receiver") != AlertingReceiverPagerDuty {
			t.Fatalf("want one PagerDuty route, got %v", routes)
		}
		m := asList(t, dig(t, routes[0], "matchers"))[0]
		if dig(t, m, "name") != "severity" || dig(t, m, "value") != tc.want {
			t.Errorf("severities %v: PagerDuty matcher = %v, want severity=~%q", tc.sev, m, tc.want)
		}
	}
}

// LLZ'S SLACK ROUTE NEVER CARRIES WHAT apl-core ALREADY POSTS: critical (its
// critical route) and CPUThrottlingHigh (routed by name). Either would post
// twice. Slack critical-only needs no CR at all.
func TestLLZSlackRouteNeverDuplicatesAplCore(t *testing.T) {
	routes := asList(t, dig(t, renderedAlertingDocs(t, Alerting{Receivers: []string{"slack"}})[slackCR], "spec", "route", "routes"))
	if len(routes) != 1 {
		t.Fatalf("want one Slack route, got %d", len(routes))
	}
	var sev, excl string
	for _, m := range asList(t, dig(t, routes[0], "matchers")) {
		switch dig(t, m, "name") {
		case "severity":
			sev = dig(t, m, "value").(string)
		case "alertname":
			if dig(t, m, "matchType") == "!~" {
				excl = dig(t, m, "value").(string)
			}
		}
	}
	if regexp.MustCompile(`^(?:` + sev + `)$`).MatchString(SeverityCritical) {
		t.Errorf("LLZ's Slack route matches severity=critical (%q) — apl-core's critical route posts it too", sev)
	}
	if sev != "warning" {
		t.Errorf("default Slack severities beyond critical = %q, want warning", sev)
	}
	if !strings.Contains(excl, "CPUThrottlingHigh") {
		t.Errorf("LLZ's Slack route does not exclude CPUThrottlingHigh, which apl-core posts by name")
	}
	if _, ok := renderedAlertingDocs(t, Alerting{Receivers: []string{"slack"}, Slack: AlertingSlack{Severities: []string{"critical"}}})[slackCR]; ok {
		t.Error("slack severities [critical] rendered an LLZ Slack CR — apl-core alone carries critical")
	}
}

// THE PROBES' "EXACTLY N PATHS" MUST BE WHAT THE RENDER DOES. A split contract:
// the gate's per-probe integration counts are derived from the spec, and the
// render is derived from the spec, and nothing else made them agree. This
// evaluates the RENDERED CRs' matchers against each probe's labels, adds
// apl-core's one Slack route that can match a probe (severity=critical →
// critical, when Slack is on), and requires the count the probe will demand.
func TestRoutingProbeCountsMatchTheRenderedRoutes(t *testing.T) {
	for _, a := range []Alerting{
		{Receivers: []string{"pagerduty"}},
		{Receivers: []string{"slack"}},
		alertingFixture(),
		{Receivers: []string{"pagerduty", "slack"}, PagerDuty: AlertingPagerDuty{Severities: []string{"critical", "warning"}}},
		{Receivers: []string{"slack"}, Slack: AlertingSlack{Severities: []string{"critical"}}},
	} {
		docs := map[string]map[string]any{}
		if RenderAlertingManifest(a) != "" {
			docs = renderedAlertingDocs(t, a)
		}
		for _, p := range AlertRoutingProbes(a) {
			labels := map[string]string{"alertname": "LLZAlertRoutingProbe"}
			for k, v := range p.Labels {
				labels[k] = v
			}
			got := map[string]int{}
			for key, amc := range docs {
				if !strings.HasPrefix(key, "AlertmanagerConfig/") {
					continue
				}
				route := dig(t, amc, "spec", "route")
				if !matchersMatch(t, dig(t, route, "matchers"), labels) {
					continue
				}
				recvs := map[string]map[string]any{}
				for _, r := range asList(t, dig(t, amc, "spec", "receivers")) {
					recvs[dig(t, r, "name").(string)] = r.(map[string]any)
				}
				for _, sub := range asList(t, dig(t, route, "routes")) {
					if !matchersMatch(t, dig(t, sub, "matchers"), labels) {
						continue
					}
					r := recvs[dig(t, sub, "receiver").(string)]
					if _, ok := r["pagerdutyConfigs"]; ok {
						got[IntegrationPagerDuty]++
					}
					if _, ok := r["slackConfigs"]; ok {
						got[IntegrationSlack]++
					}
					if c, _ := sub.(map[string]any)["continue"].(bool); !c {
						break
					}
				}
			}
			if a.HasReceiver("slack") && labels["severity"] == SeverityCritical {
				got[IntegrationSlack]++ // apl-core's critical route
			}
			for _, integ := range []string{IntegrationPagerDuty, IntegrationSlack} {
				if got[integ] != p.Integrations[integ] {
					t.Errorf("receivers %v, probe %q: the rendered routes give %d %s path(s), the gate demands %d",
						a.Receivers, p.Name, got[integ], integ, p.Integrations[integ])
				}
			}
		}
	}
}

// matchersMatch evaluates rendered AlertmanagerConfig matchers the way
// Alertmanager does: every matcher must hold, regexes anchored, an absent label
// is "".
func matchersMatch(t *testing.T, ms any, labels map[string]string) bool {
	t.Helper()
	l, _ := ms.([]any)
	for _, m := range l {
		v := labels[dig(t, m, "name").(string)]
		want := dig(t, m, "value").(string)
		var ok bool
		switch dig(t, m, "matchType") {
		case "=", nil:
			ok = v == want
		case "!=":
			ok = v != want
		case "=~":
			ok = regexp.MustCompile(`^(?:` + want + `)$`).MatchString(v)
		case "!~":
			ok = !regexp.MustCompile(`^(?:` + want + `)$`).MatchString(v)
		default:
			t.Fatalf("unknown matchType %v", dig(t, m, "matchType"))
		}
		if !ok {
			return false
		}
	}
	return true
}

// PAGERDUTY'S EVENT SEVERITY FOLLOWS THE ALERT'S. Unset, Alertmanager sends every
// event as "error", so PagerDuty's urgency rules cannot tell a critical page from
// a warning one. Executed as the Go template Alertmanager will run it.
func TestPagerDutyEventSeverityFollowsTheAlert(t *testing.T) {
	var sev string
	for _, r := range asList(t, dig(t, renderedAlertingDocs(t, Alerting{Receivers: []string{"pagerduty"}})[pdCR], "spec", "receivers")) {
		if dig(t, r, "name") == AlertingReceiverPagerDuty {
			sev, _ = dig(t, asList(t, dig(t, r, "pagerdutyConfigs"))[0], "severity").(string)
		}
	}
	if sev == "" {
		t.Fatal("the PagerDuty receiver sets no severity — every page arrives as \"error\"")
	}
	tmpl, err := template.New("sev").Parse(sev)
	if err != nil {
		t.Fatalf("severity is not a valid Go template: %v", err)
	}
	for in, want := range map[string]string{"critical": "critical", "error": "error", "warning": "warning", "info": "info", "page": "critical", "": "critical"} {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, map[string]any{"CommonLabels": map[string]string{"severity": in}}); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if buf.String() != want {
			t.Errorf("severity label %q → PagerDuty %q, want %q", in, buf.String(), want)
		}
	}
	// The group must be single-severity for CommonLabels.severity to be set.
	if gb := asList(t, dig(t, renderedAlertingDocs(t, Alerting{Receivers: []string{"pagerduty"}})[pdCR], "spec", "route", "groupBy")); !slices.Contains(gb, any("severity")) {
		t.Errorf("groupBy %v omits severity — a mixed group has no common severity label", gb)
	}
}

// The EU endpoint (or any override) must reach the receiver; unset must not
// render a url at all, so Alertmanager's own default applies.
func TestPagerDutyURLOverride(t *testing.T) {
	recv := func(a Alerting) map[string]any {
		for _, r := range asList(t, dig(t, renderedAlertingDocs(t, a)[pdCR], "spec", "receivers")) {
			if dig(t, r, "name") == AlertingReceiverPagerDuty {
				return asList(t, dig(t, r, "pagerdutyConfigs"))[0].(map[string]any)
			}
		}
		t.Fatal("no pagerduty receiver")
		return nil
	}
	if u, ok := recv(Alerting{Receivers: []string{"pagerduty"}})["url"]; ok {
		t.Errorf("an unset url rendered %v", u)
	}
	const eu = "https://events.eu.pagerduty.com/v2/enqueue"
	if u := recv(Alerting{Receivers: []string{"pagerduty"}, PagerDuty: AlertingPagerDuty{URL: eu}})["url"]; u != eu {
		t.Errorf("url = %v, want %s", u, eu)
	}
}

// `llz render --check` compares byte-for-byte; a map range in the renderer
// would make it fail intermittently on every instance.
func TestAlertingRenderIsDeterministic(t *testing.T) {
	a := Alerting{Receivers: []string{"slack", "pagerduty"}, PagerDuty: AlertingPagerDuty{Severities: []string{"critical", "page"}}}
	first, firstSet := RenderAlertingManifest(a), RenderAlertSetOverlay(a)
	for range 20 {
		if RenderAlertingManifest(a) != first || RenderAlertSetOverlay(a) != firstSet {
			t.Fatal("the alerting render is not deterministic")
		}
	}
}

// THE PROBES PIN THE EXCLUSION TOO. A gate that only proves "routes more" is
// one bad edit from a pager that fires on everything.
func TestRoutingProbesIncludeANegativeAndAnUnlabelledCase(t *testing.T) {
	for _, a := range []Alerting{{Receivers: []string{"pagerduty"}}, {Receivers: []string{"slack"}}, alertingFixture()} {
		neg, unlabelled := false, false
		for _, p := range AlertRoutingProbes(a) {
			if p.Integrations[IntegrationPagerDuty] == 0 && p.Integrations[IntegrationSlack] == 0 {
				neg = true
				if len(p.MustReach) != 0 {
					t.Errorf("negative probe %q must reach %v", p.Name, p.MustReach)
				}
			}
			if _, ok := p.Labels["namespace"]; !ok {
				unlabelled = true
			}
			if _, ok := p.Integrations[IntegrationSlack]; !ok {
				t.Errorf("probe %q does not pin the Slack count — a spec without slack would not prove apl-core is quiet", p.Name)
			}
		}
		if !neg {
			t.Errorf("receivers %v: no must-notify-nobody probe", a.Receivers)
		}
		if !unlabelled {
			t.Errorf("receivers %v: no probe without a namespace label — cluster-scoped alerts would go unchecked", a.Receivers)
		}
	}
	// Critical Slack is apl-core's receiver, warning is LLZ's: the probe names
	// the specific path, not just "some Slack".
	for _, p := range AlertRoutingProbes(Alerting{Receivers: []string{"slack"}}) {
		switch p.Labels["severity"] {
		case SeverityCritical:
			if !slices.Contains(p.MustReach, AplReceiverCritical) {
				t.Errorf("critical probe must reach apl-core's %q, got %v", AplReceiverCritical, p.MustReach)
			}
		case "warning":
			if !slices.Contains(p.MustReach, AlertingReceiverFQN(AlertingSlackConfigName, AlertingReceiverSlack)) {
				t.Errorf("warning probe must reach LLZ's Slack receiver, got %v", p.MustReach)
			}
		}
	}
}

func TestAlertingOpenBaoRefsFollowTheReceivers(t *testing.T) {
	if got := AlertingOpenBaoRefs(Alerting{Receivers: []string{"pagerduty"}}); !slices.Equal(got, []string{PagerDutyOpenBaoPath}) {
		t.Errorf("pagerduty refs = %v", got)
	}
	if got := AlertingOpenBaoRefs(Alerting{Receivers: []string{"slack"}}); !slices.Equal(got, []string{SlackOpenBaoPath}) {
		t.Errorf("slack refs = %v", got)
	}
	// And the rendered ExternalSecrets read exactly these, no more.
	var keys []string
	for key, d := range renderedAlertingDocs(t, alertingFixture()) {
		if strings.HasPrefix(key, "ExternalSecret/") {
			for _, e := range asList(t, dig(t, d, "spec", "data")) {
				keys = append(keys, dig(t, e, "remoteRef", "key").(string))
			}
		}
	}
	slices.Sort(keys)
	want := AlertingOpenBaoRefs(alertingFixture())
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("ExternalSecrets read %v, AlertingOpenBaoRefs says %v", keys, want)
	}
}

// ENV OVERRIDES MERGE FIELD BY FIELD, AND AN OPT-OUT DROPS WHAT IT OPTED OUT OF.
func TestMergeAlerting(t *testing.T) {
	base := Alerting{
		Receivers: []string{"pagerduty", "slack"},
		PagerDuty: AlertingPagerDuty{Severities: []string{"critical", "page"}, URL: "https://events.eu.pagerduty.com/v2/enqueue"},
		Slack:     AlertingSlack{Channel: "prod-alerts"},
	}
	if got := MergeAlerting(base, nil); !slices.Equal(got.Receivers, base.Receivers) || got.PagerDuty.URL != base.PagerDuty.URL {
		t.Errorf("no override must inherit the instance whole, got %+v", got)
	}
	dev := MergeAlerting(base, &Alerting{Receivers: []string{"none"}})
	if dev.Active() || dev.PagerDuty.URL != "" || dev.Slack.Channel != "" {
		t.Errorf("an env that opts out must carry nothing of the instance's receivers, got %+v", dev)
	}
	stg := MergeAlerting(base, &Alerting{Receivers: []string{"slack"}, Slack: AlertingSlack{Channel: "staging-alerts"}})
	if stg.HasReceiver("pagerduty") || stg.Slack.Channel != "staging-alerts" {
		t.Errorf("staging override = %+v", stg)
	}
	tuned := MergeAlerting(base, &Alerting{PagerDuty: AlertingPagerDuty{Severities: []string{"critical"}}})
	if !slices.Equal(tuned.Receivers, base.Receivers) || !slices.Equal(tuned.PagerDuty.Severities, []string{"critical"}) || tuned.PagerDuty.URL != base.PagerDuty.URL {
		t.Errorf("a tune-only override must keep the inherited receivers and URL, got %+v", tuned)
	}
	// The merge must not alias the instance's slice.
	tuned.Receivers[0] = "x"
	if base.Receivers[0] != "pagerduty" {
		t.Error("MergeAlerting aliased the instance's receivers")
	}
}

func TestSlackChannelDefaultsAndStripsHash(t *testing.T) {
	if c := (Alerting{}).slackChannel(); c != "mon-apl" {
		t.Errorf("default = %q", c)
	}
	if c := (Alerting{Slack: AlertingSlack{Channel: "#a"}}).slackChannel(); c != "a" {
		t.Errorf("got %q — a leading # must not render as ##a", c)
	}
}
