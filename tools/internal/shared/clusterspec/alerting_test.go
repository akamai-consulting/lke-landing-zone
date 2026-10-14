package clusterspec

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// renderedAlertingDocs decodes RenderAlertingManifest's output into its
// documents, keyed by kind. A test that string-matched the YAML would pass on
// a manifest kubectl could not even parse.
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
		kind, _ := doc["kind"].(string)
		if _, dup := out[kind]; dup {
			t.Fatalf("two %s documents rendered", kind)
		}
		out[kind] = doc
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

// ROUTES NOWHERE → RENDERS NOTHING. The observability kustomization only lists
// the file when there is one, so an instance without alerting sees no drift.
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
}

// THE CR MUST LIVE WHERE THE MATCHER EXEMPTION APPLIES. The apl-overlay sets
// OnNamespaceExceptForAlertmanagerNamespace, which exempts only CRs in
// Alertmanager's own namespace from the namespace="<cr ns>" matcher. Move the CR
// (or change the strategy) and it silently routes only alerts from one
// namespace — so both halves are read from what is actually rendered.
func TestAlertingCRLivesWhereTheMatcherExemptionApplies(t *testing.T) {
	docs := renderedAlertingDocs(t, Alerting{Receivers: []string{"pagerduty"}})
	amc := docs["AlertmanagerConfig"]
	if amc == nil {
		t.Fatal("no AlertmanagerConfig rendered")
	}
	if ns := dig(t, amc, "metadata", "namespace"); ns != AlertmanagerNamespace {
		t.Errorf("AlertmanagerConfig is in %v, not Alertmanager's namespace %q — the operator would add "+
			"namespace=%v to its route and it would page only for that namespace", ns, AlertmanagerNamespace, ns)
	}
	rv, ok := AplAppRawValues()["alertmanager"]
	if !ok {
		t.Fatal("the apl-overlay asserts nothing on apps.alertmanager — the CR's routes would carry a namespace matcher")
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

func alertingFixture() Alerting {
	return Alerting{Receivers: []string{"pagerduty", "slack"}}
}

// EVERY ROUTE NAMES A RECEIVER THE CR DECLARES, and every Secret key a receiver
// references is one the ExternalSecret writes. The operator rejects the whole CR
// on either mistake, which from the pager's side is indistinguishable from not
// having configured one.
func TestAlertingRoutesAndSecretsAreInternallyConsistent(t *testing.T) {
	for _, recv := range [][]string{{"pagerduty"}, {"slack"}, {"pagerduty", "slack"}} {
		t.Run(strings.Join(recv, "+"), func(t *testing.T) {
			docs := renderedAlertingDocs(t, Alerting{Receivers: recv})
			es, amc := docs["ExternalSecret"], docs["AlertmanagerConfig"]
			if es == nil || amc == nil {
				t.Fatalf("want an ExternalSecret and an AlertmanagerConfig, got %d document(s)", len(docs))
			}
			if dig(t, es, "metadata", "namespace") != AlertmanagerNamespace {
				t.Error("the ExternalSecret writes its Secret outside the CR's namespace — the operator resolves Secret refs in the CR's own namespace")
			}
			target := dig(t, es, "spec", "target", "name")
			written := map[string]bool{}
			for _, d := range asList(t, dig(t, es, "spec", "data")) {
				written[dig(t, d, "secretKey").(string)] = true
			}

			declared := map[string]bool{}
			for _, r := range asList(t, dig(t, amc, "spec", "receivers")) {
				name := dig(t, r, "name").(string)
				declared[name] = true
				for _, field := range []struct{ list, ref string }{{"pagerdutyConfigs", "routingKey"}, {"slackConfigs", "apiURL"}} {
					cfgs, _ := r.(map[string]any)[field.list].([]any)
					for _, c := range cfgs {
						sel := dig(t, c, field.ref)
						if dig(t, sel, "name") != target {
							t.Errorf("receiver %s references Secret %v, the ExternalSecret writes %v", name, dig(t, sel, "name"), target)
						}
						if k := dig(t, sel, "key").(string); !written[k] {
							t.Errorf("receiver %s needs Secret key %q, which the ExternalSecret never writes", name, k)
						}
					}
				}
			}
			route := dig(t, amc, "spec", "route")
			if !declared[dig(t, route, "receiver").(string)] {
				t.Errorf("the top-level route's receiver %v is not declared", dig(t, route, "receiver"))
			}
			for _, r := range asList(t, dig(t, route, "routes")) {
				if n := dig(t, r, "receiver").(string); !declared[n] {
					t.Errorf("a route sends to %q, which no receiver declares", n)
				}
			}
			// The gate looks for the expected receivers by their loaded names, so
			// each one must be a receiver this CR actually declares.
			for fqn := range AlertingExpectedReceivers(Alerting{Receivers: recv}) {
				local := strings.TrimPrefix(fqn, AlertmanagerNamespace+"/"+AlertingConfigName+"/")
				if !declared[local] {
					t.Errorf("the gate expects %s, but the CR declares no receiver %q", fqn, local)
				}
			}
		})
	}
}

// A PAGER THAT FIRES FOREVER IS AN OUTAGE TOO. Watchdog is always firing by
// design; our route sits above apl-core's (which sends it to null), so it is our
// exclusion or nobody's.
func TestAlwaysFiringAlertsAreExcludedFromEveryReceiver(t *testing.T) {
	docs := renderedAlertingDocs(t, alertingFixture())
	matchers := asList(t, dig(t, docs["AlertmanagerConfig"], "spec", "route", "matchers"))
	found := false
	for _, m := range matchers {
		if dig(t, m, "name") == "alertname" && dig(t, m, "matchType") == "!~" {
			v := dig(t, m, "value").(string)
			found = strings.Contains(v, "Watchdog") && strings.Contains(v, "InfoInhibitor")
		}
	}
	if !found {
		t.Errorf("the top-level route does not exclude Watchdog/InfoInhibitor: %v", matchers)
	}
}

// PAGERDUTY PAGES ON THE DECLARED SEVERITIES AND NOTHING WIDER, and continues,
// so a critical alert also reaches Slack when both are configured.
func TestPagerDutyRouteMatchesTheDeclaredSeverities(t *testing.T) {
	cases := []struct {
		sev  []string
		want string
	}{
		{nil, "critical"},
		{[]string{"critical", "page"}, "critical|page"},
	}
	for _, tc := range cases {
		a := Alerting{Receivers: []string{"pagerduty", "slack"}, PagerDuty: AlertingPagerDuty{Severities: tc.sev}}
		routes := asList(t, dig(t, renderedAlertingDocs(t, a)["AlertmanagerConfig"], "spec", "route", "routes"))
		first := routes[0]
		if dig(t, first, "receiver") != AlertingReceiverPagerDuty {
			t.Fatalf("the PagerDuty route is not first — a Slack route without continue would swallow it")
		}
		if dig(t, first, "continue") != true {
			t.Error("the PagerDuty route does not continue — Slack would never see a paged alert")
		}
		m := asList(t, dig(t, first, "matchers"))[0]
		if dig(t, m, "name") != "severity" || dig(t, m, "value") != tc.want {
			t.Errorf("severities %v: PagerDuty matcher = %v, want severity=~%q", tc.sev, m, tc.want)
		}
	}
}

// The EU endpoint (or any override) must reach the receiver; unset must not
// render a url at all, so Alertmanager's own default applies.
func TestPagerDutyURLOverride(t *testing.T) {
	recv := func(a Alerting) map[string]any {
		for _, r := range asList(t, dig(t, renderedAlertingDocs(t, a)["AlertmanagerConfig"], "spec", "receivers")) {
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
	first := RenderAlertingManifest(a)
	for range 20 {
		if RenderAlertingManifest(a) != first {
			t.Fatal("RenderAlertingManifest is not deterministic")
		}
	}
}

// THE PROBES PIN THE EXCLUSION TOO. A gate that only proves "routes more" is
// one bad edit from a pager that fires on everything.
func TestRoutingProbesIncludeANegative(t *testing.T) {
	probes := AlertRoutingProbes(Alerting{Receivers: []string{"pagerduty"}})
	pos, neg, unlabelled := 0, 0, 0
	for _, p := range probes {
		if p.Receiver != AlertingReceiverFQN(AlertingReceiverPagerDuty) {
			t.Errorf("probe %q targets %s", p.Name, p.Receiver)
		}
		if p.MustRoute {
			pos++
		} else {
			neg++
		}
		if _, ok := p.Labels["namespace"]; !ok {
			unlabelled++
		}
	}
	if pos == 0 || neg == 0 {
		t.Errorf("pagerduty probes: %d must-route, %d must-not — need both", pos, neg)
	}
	if unlabelled == 0 {
		t.Error("no probe without a namespace label — cluster-scoped alerts (node, apiserver) would go unchecked")
	}
	// A spec that pages on info has no negative to probe with; the probe set must
	// not then assert that info does NOT page.
	for _, p := range AlertRoutingProbes(Alerting{Receivers: []string{"pagerduty"}, PagerDuty: AlertingPagerDuty{Severities: []string{"info"}}}) {
		if !p.MustRoute && p.Labels["severity"] == "info" {
			t.Error("severities [info] still probes that info must NOT page — the gate would fail a correct cluster")
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
	// And the rendered ExternalSecret reads exactly these, no more.
	docs := renderedAlertingDocs(t, alertingFixture())
	var keys []string
	for _, d := range asList(t, dig(t, docs["ExternalSecret"], "spec", "data")) {
		keys = append(keys, dig(t, d, "remoteRef", "key").(string))
	}
	if !slices.Equal(keys, AlertingOpenBaoRefs(alertingFixture())) {
		t.Errorf("ExternalSecret reads %v, AlertingOpenBaoRefs says %v", keys, AlertingOpenBaoRefs(alertingFixture()))
	}
}

func TestSlackChannelsDefaultAndStripHash(t *testing.T) {
	def, crit := Alerting{}.slackChannels()
	if def != "mon-apl" || crit != "mon-apl-crit" {
		t.Errorf("defaults = %q/%q", def, crit)
	}
	def, crit = Alerting{Slack: AlertingSlack{Channel: "#a", ChannelCrit: "b"}}.slackChannels()
	if def != "a" || crit != "b" {
		t.Errorf("got %q/%q — a leading # must not render as ##a", def, crit)
	}
}
