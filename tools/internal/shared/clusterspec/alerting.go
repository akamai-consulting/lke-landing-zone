package clusterspec

// alerting.go renders spec.alerting into an AlertmanagerConfig CR plus the
// ExternalSecret that feeds it — the LLZ-owned channel into the managed
// platform's Alertmanager that docs/upstream-asks.md §4 said did not exist.
//
// WHY AN AlertmanagerConfig AND NOT apl-core's `alerts:` BLOCK. apl-core builds
// Alertmanager's base config (the alertmanager-platform-config Secret) from a
// TOP-LEVEL `alerts:` values block, which no channel LLZ has can reach. But the
// Alertmanager CR it deploys (kube-prometheus-stack, prometheus-operator v0.93)
// leaves both selectors at the chart default `{}`:
//
//	alertmanagerConfigSelector:          {}   → every AlertmanagerConfig …
//	alertmanagerConfigNamespaceSelector: {}   → … in every namespace
//
// and the operator MERGES each selected AlertmanagerConfig into that base config
// as a sub-route at the TOP of the root route, with `continue: true`. So a CR in
// git reaches Alertmanager without touching apl-core's values at all, and runs
// before apl-core's own `null` routes rather than behind them.
//
// THE ONE THING THAT MAKES IT ROUTE ANYTHING. By default the operator also adds
// a `namespace="<the CR's namespace>"` matcher to every AlertmanagerConfig's
// route (alertmanagerConfigMatcherStrategy: OnNamespace). For a CR in
// `monitoring` that means only alerts whose `namespace` label is `monitoring`
// are routed — OpenBao's alerts (llz-openbao), node alerts (no namespace) and
// almost everything else fall through to apl-core's null route, silently. The
// CR would look correct, load cleanly, show its receiver in Alertmanager's
// config, and page for a fraction of what fires. The apl-overlay therefore sets
// the strategy to OnNamespaceExceptForAlertmanagerNamespace through
// apps.alertmanager._rawValues (overlay_appvalues.go), which exempts exactly
// CRs in Alertmanager's own namespace — this one — and leaves every other
// namespace's CRs scoped as before. The two halves are held together by
// TestAlertingCRLivesWhereTheMatcherExemptionApplies; what holds them to the
// CLUSTER is `llz ci assert-alert-routing`, which posts silenced probe alerts
// from a non-monitoring namespace and reads back the receivers Alertmanager
// itself chose.
//
// THE SECRET HALF. Routing keys and webhook URLs never enter git or values:
// the CR names a Secret, and an ExternalSecret materialises it from OpenBao
// through the `openbao` ClusterSecretStore, the same flow every other platform
// credential uses. If the key is not seeded the ExternalSecret errors, the
// Secret never appears, and the operator REJECTS the AlertmanagerConfig (it
// resolves every SecretKeySelector at reconcile time) — so a missing key shows
// up as a missing receiver in the gate rather than as a receiver with an empty
// key.

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// AlertmanagerNamespace is where apl-core runs its Alertmanager, and where
	// the AlertmanagerConfig MUST live for the matcher exemption to apply to it.
	AlertmanagerNamespace = "monitoring"

	// AlertingConfigName names the AlertmanagerConfig, its ExternalSecret and
	// the Secret that ExternalSecret writes. One name for all three so an
	// operator reading any one of them can find the other two.
	AlertingConfigName = "llz-alerting"

	// AlertingFile is the rendered file under the observability carved app's
	// source root (apl-values/<env>/apps/observability/).
	AlertingFile = "alerting.yaml"

	// AlertmanagerConfigMatcherStrategy is the strategy the apl-overlay asserts
	// on apl-core's Alertmanager CR. See the file header for why the default
	// (OnNamespace) would route almost nothing from a CR in `monitoring`.
	AlertmanagerConfigMatcherStrategy = "OnNamespaceExceptForAlertmanagerNamespace"

	// PagerDutyOpenBaoPath / PagerDutyOpenBaoProperty are where the operator
	// seeds the PagerDuty Events API v2 routing (integration) key.
	PagerDutyOpenBaoPath     = "alerts/pagerduty"
	PagerDutyOpenBaoProperty = "routing_key"

	// SlackOpenBaoPath / SlackOpenBaoProperty are the Slack incoming-webhook URL,
	// at the path the docs have always told operators to seed.
	SlackOpenBaoPath     = "alerts/webhooks"
	SlackOpenBaoProperty = "slack_url"

	// Keys inside the materialised Secret.
	alertingSecretKeyPagerDuty = "pagerduty-routing-key"
	alertingSecretKeySlack     = "slack-url"

	// Receiver names inside the AlertmanagerConfig. The operator namespaces them
	// as <namespace>/<cr-name>/<receiver> in the config Alertmanager loads; see
	// AlertingReceiverFQN.
	AlertingReceiverPagerDuty     = "pagerduty"
	AlertingReceiverSlackCritical = "slack-critical"
	AlertingReceiverSlackDefault  = "slack-default"
	alertingReceiverUnrouted      = "unrouted"

	// Default Slack channels when spec.alerting.slack leaves them unset.
	defaultSlackChannel     = "mon-apl"
	defaultSlackChannelCrit = "mon-apl-crit"
)

// defaultPagerDutySeverities is what pages when spec.alerting.pagerduty
// .severities is unset. critical only: a pager that also fires on `warning`
// trains its on-call to ignore it, and every rule this repo ships uses
// `critical` for "a human must act now".
var defaultPagerDutySeverities = []string{"critical"}

// alwaysUnrouted are alerts that exist to fire FOREVER. kube-prometheus-stack's
// Watchdog is a dead-man's switch that is always firing by design, and
// InfoInhibitor exists only to inhibit info-level alerts. Routing either to a
// pager pages someone permanently — apl-core's own base config sends Watchdog
// to "null" for the same reason, but our route sits ABOVE apl-core's (the
// operator prepends it), so its exclusion does not protect us.
var alwaysUnrouted = []string{"Watchdog", "InfoInhibitor"}

// HasReceiver reports whether spec.alerting.receivers names r.
func (a Alerting) HasReceiver(r string) bool { return slices.Contains(a.Receivers, r) }

// Active reports whether spec.alerting routes anywhere at all. "none" and an
// empty list are the same thing: alerts aggregate in Alertmanager and notify
// nobody.
func (a Alerting) Active() bool {
	return a.HasReceiver("pagerduty") || a.HasReceiver("slack")
}

// PagerDutySeverities is the effective set of `severity` label values that page.
func (a Alerting) PagerDutySeverities() []string {
	if len(a.PagerDuty.Severities) == 0 {
		return append([]string(nil), defaultPagerDutySeverities...)
	}
	return append([]string(nil), a.PagerDuty.Severities...)
}

func (a Alerting) slackChannels() (def, crit string) {
	def, crit = a.Slack.Channel, a.Slack.ChannelCrit
	if def == "" {
		def = defaultSlackChannel
	}
	if crit == "" {
		crit = defaultSlackChannelCrit
	}
	return strings.TrimPrefix(def, "#"), strings.TrimPrefix(crit, "#")
}

// AlertingReceiverFQN is the receiver name Alertmanager actually loads for one
// of this CR's receivers. prometheus-operator rewrites every AlertmanagerConfig
// receiver to <namespace>/<name>/<receiver> so CRs from different namespaces
// cannot collide; the gate looks for exactly this string, so if the operator
// ever changes the scheme the gate fails rather than passing on nothing.
func AlertingReceiverFQN(receiver string) string {
	return AlertmanagerNamespace + "/" + AlertingConfigName + "/" + receiver
}

// AlertRoutingProbe is one synthetic alert the routing gate posts, and what
// Alertmanager must decide about it.
type AlertRoutingProbe struct {
	// Name is a short human label for the probe's verdict line.
	Name string
	// Labels are the probe's labels, EXCLUDING alertname and the per-run probe id
	// the gate adds itself.
	Labels map[string]string
	// Receiver is a fully-qualified receiver name (AlertingReceiverFQN).
	Receiver string
	// MustRoute: true requires Receiver among the alert's receivers, false
	// forbids it. The negative probes keep "routes more" from passing as
	// "routes correctly" — a pager that fires on everything is its own outage.
	MustRoute bool
}

// AlertRoutingProbeNamespace is the `namespace` label the probes carry. It is
// deliberately NOT `monitoring`: under the operator's default matcher strategy
// only alerts labelled with the CR's own namespace route, so a probe from
// `monitoring` would pass on exactly the regression it exists to catch.
const AlertRoutingProbeNamespace = "llz-alert-routing-probe"

// AlertRoutingProbes derives, from the SPEC, what the live Alertmanager must do.
// The gate never derives its expectations from the cluster it is grading.
//
// Empty when spec.alerting routes nowhere.
func AlertRoutingProbes(a Alerting) []AlertRoutingProbe {
	var out []AlertRoutingProbe
	if a.HasReceiver("pagerduty") {
		pd := AlertingReceiverFQN(AlertingReceiverPagerDuty)
		sev := a.PagerDutySeverities()
		for _, s := range sev {
			out = append(out, AlertRoutingProbe{
				Name:      "severity=" + s + " in a non-monitoring namespace pages PagerDuty",
				Labels:    map[string]string{"severity": s, "namespace": AlertRoutingProbeNamespace},
				Receiver:  pd,
				MustRoute: true,
			})
		}
		// Cluster-scoped alerts (node, apiserver) carry no namespace label at all;
		// a route that only matched namespaced alerts would miss every one.
		out = append(out, AlertRoutingProbe{
			Name:      "severity=" + sev[0] + " with no namespace label pages PagerDuty",
			Labels:    map[string]string{"severity": sev[0]},
			Receiver:  pd,
			MustRoute: true,
		})
		if !slices.Contains(sev, "info") {
			out = append(out, AlertRoutingProbe{
				Name:      "severity=info does NOT page PagerDuty",
				Labels:    map[string]string{"severity": "info", "namespace": AlertRoutingProbeNamespace},
				Receiver:  pd,
				MustRoute: false,
			})
		}
	}
	if a.HasReceiver("slack") {
		out = append(out,
			AlertRoutingProbe{
				Name:      "severity=critical reaches the critical Slack channel",
				Labels:    map[string]string{"severity": "critical", "namespace": AlertRoutingProbeNamespace},
				Receiver:  AlertingReceiverFQN(AlertingReceiverSlackCritical),
				MustRoute: true,
			},
			AlertRoutingProbe{
				Name:      "severity=warning reaches the default Slack channel",
				Labels:    map[string]string{"severity": "warning", "namespace": AlertRoutingProbeNamespace},
				Receiver:  AlertingReceiverFQN(AlertingReceiverSlackDefault),
				MustRoute: true,
			},
		)
	}
	return out
}

// AlertingExpectedReceivers is every receiver (fully qualified) the loaded
// Alertmanager config must carry, and the secret-bearing field each one must
// have set. Derived from the spec, like the probes.
func AlertingExpectedReceivers(a Alerting) map[string]string {
	out := map[string]string{}
	if a.HasReceiver("pagerduty") {
		out[AlertingReceiverFQN(AlertingReceiverPagerDuty)] = "pagerduty_configs.routing_key"
	}
	if a.HasReceiver("slack") {
		out[AlertingReceiverFQN(AlertingReceiverSlackCritical)] = "slack_configs.api_url"
		out[AlertingReceiverFQN(AlertingReceiverSlackDefault)] = "slack_configs.api_url"
	}
	return out
}

// AlertingOpenBaoRefs lists the OpenBao KV paths (under the `secret` mount) the
// rendered ExternalSecret reads. Exported so the policy that lets ESO read them
// can be tested against what is actually rendered.
func AlertingOpenBaoRefs(a Alerting) []string {
	var out []string
	if a.HasReceiver("pagerduty") {
		out = append(out, PagerDutyOpenBaoPath)
	}
	if a.HasReceiver("slack") {
		out = append(out, SlackOpenBaoPath)
	}
	return out
}

// RenderAlertingManifest returns the ExternalSecret + AlertmanagerConfig for
// spec.alerting, or "" when it routes nowhere (nothing is rendered, and the
// observability kustomization does not reference the file).
//
// Deterministic: the receiver order is fixed, never a map range, because
// `llz render --check` compares this byte-for-byte.
func RenderAlertingManifest(a Alerting) string {
	if !a.Active() {
		return ""
	}
	var b strings.Builder
	b.WriteString(`# GENERATED by ` + "`llz render`" + ` from spec.alerting — DO NOT EDIT.
# Routes Alertmanager alerts to the receivers spec.alerting names. See
# docs/alerting.md; the gate is ` + "`llz ci assert-alert-routing`" + `.
#
# The routing key / webhook URL never enter git: the ExternalSecret below
# materialises them from OpenBao, and the AlertmanagerConfig references the
# resulting Secret. Seed with ` + "`llz openbao set`" + ` (see docs/alerting.md).
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: ` + AlertingConfigName + `
  namespace: ` + AlertmanagerNamespace + `
  annotations:
    argocd.argoproj.io/sync-wave: "5"
spec:
  # 1m, like the other platform ExternalSecrets: the openbao store only goes
  # Ready after OpenBao is unsealed, and ESO does not re-trigger on recovery.
  refreshInterval: 1m
  secretStoreRef:
    kind: ClusterSecretStore
    name: openbao
  target:
    name: ` + AlertingConfigName + `
    creationPolicy: Owner
  data:
`)
	if a.HasReceiver("pagerduty") {
		fmt.Fprintf(&b, "    - secretKey: %s\n      remoteRef:\n        key: %s\n        property: %s\n",
			alertingSecretKeyPagerDuty, PagerDutyOpenBaoPath, PagerDutyOpenBaoProperty)
	}
	if a.HasReceiver("slack") {
		fmt.Fprintf(&b, "    - secretKey: %s\n      remoteRef:\n        key: %s\n        property: %s\n",
			alertingSecretKeySlack, SlackOpenBaoPath, SlackOpenBaoProperty)
	}

	b.WriteString(`---
# Merged by prometheus-operator into apl-core's Alertmanager config as a
# sub-route at the TOP of the root route (continue: true), so it runs before
# apl-core's own null routes. It MUST stay in namespace ` + AlertmanagerNamespace + `: only
# there does the apl-overlay's ` + AlertmanagerConfigMatcherStrategy + `
# strategy spare it the namespace="` + AlertmanagerNamespace + `" matcher the operator would
# otherwise add — which would route only alerts from this one namespace.
apiVersion: monitoring.coreos.com/v1alpha1
kind: AlertmanagerConfig
metadata:
  name: ` + AlertingConfigName + `
  namespace: ` + AlertmanagerNamespace + `
  annotations:
    # After the ExternalSecret: the operator rejects a CR whose Secret is absent.
    argocd.argoproj.io/sync-wave: "10"
spec:
  route:
    receiver: ` + alertingReceiverUnrouted + `
    groupBy: [alertname, namespace]
    matchers:
      # Always-firing by design; paging on them pages forever.
      - name: alertname
        matchType: "!~"
        value: ` + strings.Join(alwaysUnrouted, "|") + `
    routes:
`)
	if a.HasReceiver("pagerduty") {
		// continue: true so a critical alert ALSO reaches Slack when both are
		// configured — the pager wakes someone, the channel keeps the record.
		fmt.Fprintf(&b, `      - receiver: %s
        matchers:
          - name: severity
            matchType: "=~"
            value: %s
        continue: true
`, AlertingReceiverPagerDuty, severityRegex(a.PagerDutySeverities()))
	}
	if a.HasReceiver("slack") {
		fmt.Fprintf(&b, `      - receiver: %s
        matchers:
          - name: severity
            matchType: "="
            value: critical
      - receiver: %s
`, AlertingReceiverSlackCritical, AlertingReceiverSlackDefault)
	}

	b.WriteString("  receivers:\n    - name: " + alertingReceiverUnrouted + "\n")
	if a.HasReceiver("pagerduty") {
		fmt.Fprintf(&b, `    - name: %s
      pagerdutyConfigs:
        - sendResolved: true
          routingKey:
            name: %s
            key: %s
`, AlertingReceiverPagerDuty, AlertingConfigName, alertingSecretKeyPagerDuty)
		if a.PagerDuty.URL != "" {
			fmt.Fprintf(&b, "          url: %q\n", a.PagerDuty.URL)
		}
	}
	if a.HasReceiver("slack") {
		def, crit := a.slackChannels()
		for _, r := range []struct{ name, channel string }{
			{AlertingReceiverSlackCritical, crit},
			{AlertingReceiverSlackDefault, def},
		} {
			fmt.Fprintf(&b, `    - name: %s
      slackConfigs:
        - sendResolved: true
          channel: %q
          apiURL:
            name: %s
            key: %s
`, r.name, "#"+r.channel, AlertingConfigName, alertingSecretKeySlack)
		}
	}
	return b.String()
}

// severityRegex anchors nothing on purpose: Alertmanager anchors every regex
// matcher itself (^(?:…)$), so "critical" cannot match "noncritical".
func severityRegex(sev []string) string {
	return strings.Join(sev, "|")
}
