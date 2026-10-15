package clusterspec

// alerting.go renders spec.alerting into the two channels that reach the managed
// platform's Alertmanager, one per receiver kind.
//
// SLACK RIDES apl-core's OWN RECEIVER. apl-core maps a settings CR,
// `kind: AplAlertSet` at env/settings/alerts.yaml, onto its `$.alerts` values
// (src/common/repo.ts getFileMaps) — the same mechanism LLZ already uses for
// AplObjectStorage at env/settings/obj.yaml. From `alerts.receivers` apl-core
// builds the alertmanager-platform-config Secret: a `default` and a `critical`
// receiver with slack_configs, the global slack_api_url_file pointing at its
// `alertmanager-credentials` Secret, and that Secret's ExternalSecret reading
// `alerts-secrets.slack_url` from `apl-secrets` through core-secrets-store
// (values/prometheus-operator/prometheus-operator-raw.gotmpl). So LLZ:
//
//   - renders the AplAlertSet into the per-env apl-overlay (RenderAlertSetOverlay),
//     which the reconciler merges KEY-LEVEL onto apl-core's file
//     (SetAlertSetSpec) — the Console can co-write the same file, exactly as
//     apl-core co-writes otomi.yaml, so a whole-file write would clobber it;
//   - materialises `alerts-secrets` in `apl-secrets` from OpenBao with an
//     ExternalSecret, exactly as obj-secrets does for object storage, so the
//     webhook URL never enters git (the AplAlertSet's `slack.url` x-secret is
//     left unset, and apl-core seals nothing).
//
// There is ONE Slack path per alert. apl-core's base config routes only
// `severity=critical` (to `critical`) and `CPUThrottlingHigh` (to `default`);
// every other alert goes to its `null` receiver. Slack severities beyond
// critical (warning, by default) therefore need a route apl-core does not
// have, and an AlertmanagerConfig route can only name receivers its own CR
// declares — so LLZ's `llz-alerting-slack` CR carries a `slack` receiver for
// exactly SlackSeverities minus critical, reading the SAME Secret apl-core mounts
// (alertmanager-credentials/slackUrl). Critical is apl-core's, never LLZ's;
// CPUThrottlingHigh is excluded from LLZ's route because apl-core already
// posts it. `llz ci assert-alert-routing` counts the Slack-bearing receivers
// Alertmanager picks for each probe and requires exactly one.
//
// PAGERDUTY RIDES AN AlertmanagerConfig (`llz-alerting`, separate from the Slack
// one — see renderSlackConfig), because apl-core has no PagerDuty receiver. apl-core's Alertmanager CR leaves alertmanagerConfigSelector and
// alertmanagerConfigNamespaceSelector at `{}`, so prometheus-operator merges
// every AlertmanagerConfig into the base config as a sub-route at the TOP of
// the root route, forced to `continue: true`. That `continue` is why a paged
// critical alert ALSO reaches apl-core's critical Slack route — a different
// target, by design (the pager wakes someone, the channel keeps the record) —
// and why nothing in apl-core's tree can reach PagerDuty a second time.
//
// THE ONE THING THAT MAKES LLZ'S CRs ROUTE ANYTHING. By default the operator adds
// `namespace="<the CR's namespace>"` to every AlertmanagerConfig's route
// (alertmanagerConfigMatcherStrategy: OnNamespace). For a CR in `monitoring`
// that pages only alerts labelled namespace=monitoring — OpenBao, node and
// apiserver alerts fall through to apl-core's null route, silently. The
// apl-overlay sets OnNamespaceExceptForAlertmanagerNamespace through
// apps.alertmanager._rawValues (overlay_appvalues.go), which exempts exactly
// CRs in Alertmanager's own namespace — LLZ's.
//
// TEAM ALERTMANAGERS ARE NOT SCOPED OUT, and LLZ has no channel that could.
// apl-core deploys a per-team Alertmanager (team-<id>) when a team sets
// managedMonitoring.alertmanager, with the same `{}` selectors, and the
// platform Prometheus sends EVERY alert to each one. Those Alertmanagers merge
// LLZ's CRs too — under their default OnNamespace strategy, so they route there
// for namespace=monitoring alerts only, and pages PagerDuty a second time for
// exactly those. LLZ's own team CRs set managedMonitoring.alertmanager: false;
// a Console-created team can turn it on. The gate reads every non-platform
// Alertmanager's running config and fails if it carries LLZ's receivers, and
// docs/upstream-asks.md §4 asks apl-core to scope team selectors to their own
// namespace.

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// AlertmanagerNamespace is where apl-core runs its Alertmanager, and where
	// the AlertmanagerConfig MUST live for the matcher exemption to apply to it.
	AlertmanagerNamespace = "monitoring"

	// AlertingConfigName names the PagerDuty AlertmanagerConfig, its
	// ExternalSecret and the Secret that ExternalSecret writes. One name for all
	// three so an operator reading any one of them can find the other two.
	AlertingConfigName = "llz-alerting"

	// AlertingSlackConfigName names the Slack AlertmanagerConfig — a separate CR
	// so a Slack secret problem cannot get the PagerDuty CR rejected with it.
	AlertingSlackConfigName = "llz-alerting-slack"

	// AlertingFile is the rendered file under the observability carved app's
	// source root (apl-values/<env>/apps/observability/).
	AlertingFile = "alerting.yaml"

	// OverlayAlertsFile is apl-core's alert settings CR in the per-env apl-overlay
	// source (apl-values/<env>/apl-overlay/alerts.yaml); the reconciler merges it
	// onto env/settings/alerts.yaml on apl-<env>.
	OverlayAlertsFile = "alerts.yaml"

	// alertSetKind / alertSetName are the CR apl-core keeps env/settings/alerts.yaml
	// as — confirmed against apl-core's own fixture,
	// tests/fixtures/env/settings/alerts.yaml.
	alertSetKind = "AplAlertSet"
	alertSetName = "alerts"

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

	// AplAlertsSecretName / AplSecretsNamespace / aplAlertsSlackProperty are the
	// Secret apl-core's alertmanager-credentials ExternalSecret reads the Slack URL
	// from (core-secrets-store → apl-secrets/alerts-secrets, property slack_url).
	// apl-core would SEAL the AplAlertSet's slack.url there; LLZ writes it from
	// OpenBao instead. The namespace is not a choice: it is the one
	// core-secrets-store reads (remoteNamespace: apl-secrets).
	AplAlertsSecretName    = "alerts-secrets"
	AplSecretsNamespace    = "apl-secrets"
	aplAlertsSlackProperty = "slack_url"

	// AplAlertmanagerCredentialsSecret / AplAlertmanagerCredentialsSlackKey are
	// where apl-core lands the Slack URL for Alertmanager to mount
	// (/etc/alertmanager/secrets/alertmanager-credentials/slackUrl). LLZ's own
	// Slack route reads the same key, so there is one Slack secret chain, not two.
	AplAlertmanagerCredentialsSecret   = "alertmanager-credentials"
	AplAlertmanagerCredentialsSlackKey = "slackUrl"

	// AplReceiverCritical is apl-core's receiver for `severity=critical`.
	AplReceiverCritical = "critical"

	// Keys inside the materialised llz-alerting Secret.
	alertingSecretKeyPagerDuty = "pagerduty-routing-key"

	// Receiver names inside the AlertmanagerConfig. The operator namespaces them
	// as <namespace>/<cr-name>/<receiver> in the config Alertmanager loads; see
	// AlertingReceiverFQN.
	AlertingReceiverPagerDuty = "pagerduty"
	AlertingReceiverSlack     = "slack"
	alertingReceiverUnrouted  = "unrouted"

	// defaultSlackChannel is apl-core's own schema default for alerts.slack.channel.
	defaultSlackChannel = "mon-apl"

	// SeverityCritical is the severity apl-core's own critical route matches.
	SeverityCritical = "critical"
)

// defaultPagerDutySeverities is what pages when spec.alerting.pagerduty
// .severities is unset. critical only: a pager that also fires on `warning`
// trains its on-call to ignore it, and every rule this repo ships uses
// `critical` for "a human must act now".
var defaultPagerDutySeverities = []string{"critical"}

// defaultSlackSeverities is what posts to Slack when spec.alerting.slack
// .severities is unset. warning AND critical: the channel is the record of
// everything a human should look at; info is not that.
var defaultSlackSeverities = []string{"warning", "critical"}

// alwaysUnrouted are alerts that exist to fire FOREVER. kube-prometheus-stack's
// Watchdog is a dead-man's switch that is always firing by design, and
// InfoInhibitor exists only to inhibit info-level alerts. Routing either pages
// someone permanently — apl-core's own base config sends Watchdog to "null" for
// the same reason, but our CR's route sits ABOVE apl-core's (the operator
// prepends it), so its exclusion does not protect us.
var alwaysUnrouted = []string{"Watchdog", "InfoInhibitor"}

// aplCoreSlackOwned are alerts apl-core's base config already posts to Slack
// itself, regardless of severity (it routes CPUThrottlingHigh to `default`).
// LLZ's Slack route skips them, or each would post twice.
var aplCoreSlackOwned = []string{"CPUThrottlingHigh"}

// HasReceiver reports whether spec.alerting.receivers names r.
func (a Alerting) HasReceiver(r string) bool { return slices.Contains(a.Receivers, r) }

// Active reports whether spec.alerting routes anywhere at all. "none" and an
// empty list are the same thing: alerts aggregate in Alertmanager and notify
// nobody.
func (a Alerting) Active() bool {
	return a.HasReceiver("pagerduty") || a.HasReceiver("slack")
}

// Declared reports whether spec.alerting says anything at all — including an
// explicit [none]. A declared spec owns apl-core's alert receivers (the
// AplAlertSet is rendered, [none] included, so a Slack receiver turned on by
// hand in the Console does not survive as a second, unspecified path); an
// undeclared one leaves apl-core's settings alone.
func (a Alerting) Declared() bool { return len(a.Receivers) > 0 }

// PagerDutySeverities is the effective set of `severity` label values that page.
func (a Alerting) PagerDutySeverities() []string {
	if len(a.PagerDuty.Severities) == 0 {
		return append([]string(nil), defaultPagerDutySeverities...)
	}
	return append([]string(nil), a.PagerDuty.Severities...)
}

// SlackSeverities is the effective set of `severity` label values that post to
// Slack. Validation guarantees it contains critical.
func (a Alerting) SlackSeverities() []string {
	if len(a.Slack.Severities) == 0 {
		return append([]string(nil), defaultSlackSeverities...)
	}
	return append([]string(nil), a.Slack.Severities...)
}

// llzSlackSeverities are the Slack severities LLZ's own route carries — every
// one but critical, which apl-core's critical receiver already posts. Empty when
// Slack is not selected or posts critical only.
func (a Alerting) llzSlackSeverities() []string {
	if !a.HasReceiver("slack") {
		return nil
	}
	var out []string
	for _, s := range a.SlackSeverities() {
		if s != SeverityCritical {
			out = append(out, s)
		}
	}
	return out
}

func (a Alerting) slackChannel() string {
	c := a.Slack.Channel
	if c == "" {
		c = defaultSlackChannel
	}
	return strings.TrimPrefix(c, "#")
}

// MergeAlerting is an env's effective alerting: the instance's spec.alerting with
// the env's override laid over it field by field — the same "a set field wins,
// an unset one inherits" rule spec.defaults follows (merge.go). Settings for a
// receiver the merged list does not select are dropped, so an env that opts out
// with `receivers: [none]` does not carry the instance's PagerDuty settings
// along as inert baggage.
func MergeAlerting(base Alerting, over *Alerting) Alerting {
	out := base
	if over != nil {
		out.Receivers = pickSlice(base.Receivers, over.Receivers)
		out.PagerDuty.Severities = pickSlice(base.PagerDuty.Severities, over.PagerDuty.Severities)
		out.PagerDuty.URL = pickStr(base.PagerDuty.URL, over.PagerDuty.URL)
		out.Slack.Channel = pickStr(base.Slack.Channel, over.Slack.Channel)
		out.Slack.ChannelCrit = pickStr(base.Slack.ChannelCrit, over.Slack.ChannelCrit)
		out.Slack.Severities = pickSlice(base.Slack.Severities, over.Slack.Severities)
	}
	out.Receivers = append([]string(nil), out.Receivers...)
	if !out.HasReceiver("pagerduty") {
		out.PagerDuty = AlertingPagerDuty{}
	}
	if !out.HasReceiver("slack") {
		out.Slack = AlertingSlack{}
	}
	return out
}

// AlertingFor is env's effective alerting (MergeAlerting over the assembled spec).
// Whether the env can CARRY it is a separate question — see AlertingApplies.
func (lz *LandingZone) AlertingFor(env string) Alerting {
	return MergeAlerting(lz.Spec.Alerting, lz.Spec.Environments[env].Alerting)
}

// ObservabilityDeployed reports whether env deploys the observability component
// — apl-core's Prometheus/Alertmanager plus LLZ's glue — the way `llz render`
// decides it: enabled AND emitted on managed (`loki` declared in managedApps).
// Both alerting channels ride it: the CRs and the ExternalSecrets live in its
// carved app, and an Alertmanager that is not deployed has nothing to route.
func ObservabilityDeployed(e Environment) bool {
	c, ok := componentByName["observability"]
	return ok && ComponentEnabled(e.Components, c.Name) && c.EmitOnManaged(e.Cluster.Bootstrap, e.Components)
}

// AlertingApplies reports whether env's effective alerting is rendered at all.
// An env without observability is SKIPPED, not failed: an instance-wide
// spec.alerting should not make a cluster that never runs Alertmanager
// unrenderable. An env that asks for a receiver itself while lacking
// observability is a validation error instead (validateEnvAlerting), because
// there the author has said something LLZ cannot do.
func AlertingApplies(e Environment) bool { return ObservabilityDeployed(e) }

// AlertingReceiverFQN is the receiver name Alertmanager actually loads for one
// of LLZ's CR receivers. prometheus-operator rewrites every AlertmanagerConfig
// receiver to <namespace>/<name>/<receiver> so CRs from different namespaces
// cannot collide; the gate looks for exactly this string, so if the operator
// ever changes the scheme the gate fails rather than passing on nothing.
func AlertingReceiverFQN(cr, receiver string) string {
	return AlertmanagerNamespace + "/" + cr + "/" + receiver
}

// IsLLZAlertingReceiver reports whether a loaded receiver name came from one of
// LLZ's AlertmanagerConfigs — how the gate recognises LLZ's receivers inside a
// TEAM Alertmanager that should not have them.
func IsLLZAlertingReceiver(name string) bool {
	for _, cr := range []string{AlertingConfigName, AlertingSlackConfigName} {
		if strings.HasPrefix(name, AlertmanagerNamespace+"/"+cr+"/") {
			return true
		}
	}
	return false
}

// Integration names, as Alertmanager's config spells the *_configs keys.
const (
	IntegrationPagerDuty = "pagerduty"
	IntegrationSlack     = "slack"
)

// AlertRoutingProbe is one synthetic alert the routing gate posts, and what
// Alertmanager must decide about it.
type AlertRoutingProbe struct {
	// Name is a short human label for the probe's verdict line.
	Name string
	// Labels are the probe's labels, EXCLUDING alertname and the per-run probe id
	// the gate adds itself.
	Labels map[string]string
	// MustReach are receivers (as Alertmanager loads them) that MUST be among the
	// alert's receivers — the specific path, so "some Slack receiver" cannot pass
	// for the one the spec implies.
	MustReach []string
	// Integrations is EXACTLY how many of the alert's receivers carry each
	// integration — 0 for "must not notify there", 1 for "exactly one path".
	// Counting, not just requiring, is what catches a duplicate: the LLZ and
	// apl-core Slack routes both matching one alert would pass a "reaches Slack"
	// check and post twice.
	Integrations map[string]int
}

// AlertRoutingProbeNamespace is the `namespace` label the probes carry. It is
// deliberately NOT `monitoring`: under the operator's default matcher strategy
// only alerts labelled with the CR's own namespace route, so a probe from
// `monitoring` would pass on exactly the regression it exists to catch.
const AlertRoutingProbeNamespace = "llz-alert-routing-probe"

// probeNegativeSeverity is a severity no receiver may notify on. Validation
// keeps info out of every LLZ severity list, and apl-core routes info to null.
const probeNegativeSeverity = "info"

// AlertRoutingProbes derives, from the SPEC, what the live Alertmanager must do.
// The gate never derives its expectations from the cluster it is grading.
//
// One probe per severity any receiver names, plus a must-not-notify info probe
// and a no-namespace probe. Every probe pins the count of BOTH integrations, so
// a spec without slack also proves apl-core is not posting to Slack behind it.
// Empty when spec.alerting routes nowhere.
func AlertRoutingProbes(a Alerting) []AlertRoutingProbe {
	if !a.Active() {
		return nil
	}
	pd := a.HasReceiver("pagerduty")
	sl := a.HasReceiver("slack")
	var sevs []string
	if pd {
		sevs = append(sevs, a.PagerDutySeverities()...)
	}
	if sl {
		for _, s := range a.SlackSeverities() {
			if !slices.Contains(sevs, s) {
				sevs = append(sevs, s)
			}
		}
	}
	probe := func(name string, labels map[string]string) AlertRoutingProbe {
		sev := labels["severity"]
		p := AlertRoutingProbe{Name: name, Labels: labels, Integrations: map[string]int{IntegrationPagerDuty: 0, IntegrationSlack: 0}}
		if pd && slices.Contains(a.PagerDutySeverities(), sev) {
			p.Integrations[IntegrationPagerDuty] = 1
			p.MustReach = append(p.MustReach, AlertingReceiverFQN(AlertingConfigName, AlertingReceiverPagerDuty))
		}
		if sl && slices.Contains(a.SlackSeverities(), sev) {
			p.Integrations[IntegrationSlack] = 1
			if sev == SeverityCritical {
				p.MustReach = append(p.MustReach, AplReceiverCritical)
			} else {
				p.MustReach = append(p.MustReach, AlertingReceiverFQN(AlertingSlackConfigName, AlertingReceiverSlack))
			}
		}
		return p
	}
	var out []AlertRoutingProbe
	for _, s := range sevs {
		out = append(out, probe("severity="+s+" in a non-monitoring namespace",
			map[string]string{"severity": s, "namespace": AlertRoutingProbeNamespace}))
	}
	// Cluster-scoped alerts (node, apiserver) carry no namespace label at all; a
	// route that only matched namespaced alerts would miss every one.
	out = append(out, probe("severity="+sevs[0]+" with no namespace label",
		map[string]string{"severity": sevs[0]}))
	// The exclusion, pinned: a pager or channel that fires on everything is its
	// own outage, and "routes more" must not pass as "routes correctly".
	out = append(out, probe("severity="+probeNegativeSeverity+" notifies nobody",
		map[string]string{"severity": probeNegativeSeverity, "namespace": AlertRoutingProbeNamespace}))
	return out
}

// ExpectedReceiver is one receiver the loaded Alertmanager config must carry,
// with the integration it must have and its credential set.
type ExpectedReceiver struct {
	Name        string // as Alertmanager loads it
	Integration string // IntegrationPagerDuty / IntegrationSlack
}

// AlertingExpectedReceivers is every receiver the loaded Alertmanager config must
// carry. Derived from the spec, like the probes; sorted by name.
func AlertingExpectedReceivers(a Alerting) []ExpectedReceiver {
	var out []ExpectedReceiver
	if a.HasReceiver("slack") {
		out = append(out, ExpectedReceiver{AplReceiverCritical, IntegrationSlack})
	}
	if a.HasReceiver("pagerduty") {
		out = append(out, ExpectedReceiver{AlertingReceiverFQN(AlertingConfigName, AlertingReceiverPagerDuty), IntegrationPagerDuty})
	}
	if len(a.llzSlackSeverities()) > 0 {
		out = append(out, ExpectedReceiver{AlertingReceiverFQN(AlertingSlackConfigName, AlertingReceiverSlack), IntegrationSlack})
	}
	slices.SortFunc(out, func(x, y ExpectedReceiver) int { return strings.Compare(x.Name, y.Name) })
	return out
}

// AlertingOpenBaoRefs lists the OpenBao KV paths (under the `secret` mount) the
// rendered ExternalSecrets read. Exported so the policy that lets ESO read them
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

// pagerDutySeverityTemplate maps the alert's severity label onto PagerDuty's
// event severity enum (critical / error / warning / info).
//
// Unset, Alertmanager sends every event as "error", so a critical page and a
// warning page look identical in PagerDuty's urgency rules. A severity outside
// the enum — an operator's own "page", say — maps to critical: it only reaches
// this receiver because the spec chose to PAGE on it. `severity` is in the CR's
// groupBy, so CommonLabels.severity is always the group's one severity rather
// than empty for a mixed group.
const pagerDutySeverityTemplate = `{{ if eq .CommonLabels.severity "critical" "error" "warning" "info" }}{{ .CommonLabels.severity }}{{ else }}critical{{ end }}`

// RenderAlertingManifest returns the observability app's alerting.yaml for an
// env's effective spec.alerting, or "" when it needs nothing there (nothing is
// rendered, and the observability kustomization does not reference the file).
//
// Up to four documents: the alerts-secrets ExternalSecret (slack), the
// llz-alerting ExternalSecret + AlertmanagerConfig (pagerduty), and the
// llz-alerting-slack AlertmanagerConfig (a Slack severity other than critical).
//
// Deterministic: the receiver order is fixed, never a map range, because
// `llz render --check` compares this byte-for-byte.
func RenderAlertingManifest(a Alerting) string {
	if !a.Active() {
		return ""
	}
	var docs []string
	if a.HasReceiver("slack") {
		docs = append(docs, `# apl-core's Slack secret, materialised from OpenBao. apl-core reads the webhook
# URL from `+"`"+AplSecretsNamespace+`/`+AplAlertsSecretName+"`"+` (core-secrets-store, property `+aplAlertsSlackProperty+`) into
# its `+AplAlertmanagerCredentialsSecret+` Secret, which Alertmanager mounts — the URL it would
# otherwise SEAL there from the AplAlertSet's slack.url, which LLZ leaves unset so
# the secret never enters git. Same pattern as obj-secrets. The namespace is not a
# choice: it is the only one core-secrets-store reads.
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: `+AplAlertsSecretName+`
  namespace: `+AplSecretsNamespace+`
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
    name: `+AplAlertsSecretName+`
    creationPolicy: Owner
  data:
    - secretKey: `+aplAlertsSlackProperty+`
      remoteRef:
        key: `+SlackOpenBaoPath+`
        property: `+SlackOpenBaoProperty+`
`)
	}
	if a.HasReceiver("pagerduty") {
		docs = append(docs, `# The PagerDuty routing key, for the AlertmanagerConfig below. If it is not
# seeded the ExternalSecret errors, the Secret never appears, and the operator
# REJECTS the AlertmanagerConfig — a missing key shows up as a missing receiver
# in the gate rather than as a receiver with an empty key.
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: `+AlertingConfigName+`
  namespace: `+AlertmanagerNamespace+`
  annotations:
    argocd.argoproj.io/sync-wave: "5"
spec:
  refreshInterval: 1m
  secretStoreRef:
    kind: ClusterSecretStore
    name: openbao
  target:
    name: `+AlertingConfigName+`
    creationPolicy: Owner
  data:
    - secretKey: `+alertingSecretKeyPagerDuty+`
      remoteRef:
        key: `+PagerDutyOpenBaoPath+`
        property: `+PagerDutyOpenBaoProperty+`
`)
	}
	if a.HasReceiver("pagerduty") {
		docs = append(docs, renderPagerDutyConfig(a))
	}
	if len(a.llzSlackSeverities()) > 0 {
		docs = append(docs, renderSlackConfig(a))
	}
	return `# GENERATED by ` + "`llz render`" + ` from spec.alerting — DO NOT EDIT.
# Wires Alertmanager to the receivers spec.alerting names. Slack is apl-core's own
# receiver (env/settings/alerts.yaml via the apl-overlay); this file carries its
# secret and the routes apl-core has no receiver for. See docs/alerting.md; the
# gate is ` + "`llz ci assert-alert-routing`" + `.
#
# The routing key / webhook URL never enter git: the ExternalSecrets below
# materialise them from OpenBao. Seed with ` + "`llz openbao set`" + ` (see docs/alerting.md).

` + strings.Join(docs, "---\n")
}

// amcRoute is one sub-route of an AlertmanagerConfig.
type amcRoute struct {
	receiver string
	matchers [][3]string // name, matchType, value
	cont     bool
}

// renderAlertmanagerConfig renders one AlertmanagerConfig: the shared top-level
// route (Watchdog/InfoInhibitor excluded) over routes, and receivers verbatim.
func renderAlertmanagerConfig(name, purpose string, routes []amcRoute, receivers string) string {
	var b strings.Builder
	b.WriteString(`# ` + purpose + `
# Merged by prometheus-operator into apl-core's Alertmanager config as a
# sub-route at the TOP of the root route (forced continue: true), so it runs
# before apl-core's own routes and they still run after it. It MUST stay in
# namespace ` + AlertmanagerNamespace + `: only there does the apl-overlay's
# ` + AlertmanagerConfigMatcherStrategy + ` strategy spare it the
# namespace="` + AlertmanagerNamespace + `" matcher the operator would otherwise add.
apiVersion: monitoring.coreos.com/v1alpha1
kind: AlertmanagerConfig
metadata:
  name: ` + name + `
  namespace: ` + AlertmanagerNamespace + `
  annotations:
    # After the ExternalSecrets: the operator rejects a CR whose Secret is absent.
    argocd.argoproj.io/sync-wave: "10"
spec:
  route:
    receiver: ` + alertingReceiverUnrouted + `
    # severity is in the key so a group never mixes severities — a notification
    # (and PagerDuty's event severity) is about ONE severity.
    groupBy: [alertname, namespace, severity]
    matchers:
      # Always-firing by design; notifying on them notifies forever.
      - name: alertname
        matchType: "!~"
        value: ` + strings.Join(alwaysUnrouted, "|") + `
    routes:
`)
	for _, r := range routes {
		fmt.Fprintf(&b, "      - receiver: %s\n        matchers:\n", r.receiver)
		for _, m := range r.matchers {
			fmt.Fprintf(&b, "          - name: %s\n            matchType: %q\n            value: %s\n", m[0], m[1], m[2])
		}
		if r.cont {
			b.WriteString("        continue: true\n")
		}
	}
	b.WriteString("  receivers:\n    - name: " + alertingReceiverUnrouted + "\n")
	b.WriteString(receivers)
	return b.String()
}

// renderPagerDutyConfig is the PagerDuty AlertmanagerConfig.
func renderPagerDutyConfig(a Alerting) string {
	recv := fmt.Sprintf(`    - name: %s
      pagerdutyConfigs:
        - sendResolved: true
          routingKey:
            name: %s
            key: %s
          severity: '%s'
`, AlertingReceiverPagerDuty, AlertingConfigName, alertingSecretKeyPagerDuty, pagerDutySeverityTemplate)
	if a.PagerDuty.URL != "" {
		recv += fmt.Sprintf("          url: %q\n", a.PagerDuty.URL)
	}
	return renderAlertmanagerConfig(AlertingConfigName,
		"PagerDuty — apl-core has no PagerDuty receiver, so this route is LLZ's.",
		[]amcRoute{{
			receiver: AlertingReceiverPagerDuty,
			matchers: [][3]string{{"severity", "=~", severityRegex(a.PagerDutySeverities())}},
		}}, recv)
}

// renderSlackConfig is the AlertmanagerConfig for the Slack severities apl-core
// has no route for.
//
// A SEPARATE CR FROM PAGERDUTY'S, on purpose. It reads apl-core's
// alertmanager-credentials Secret, which exists only once apl-core has applied
// the AplAlertSet; the operator rejects a CR whose Secret is absent, and a
// rejected CR drops EVERY route in it. In one CR, a Slack secret that had not
// landed yet — or a Slack webhook rotated away — would silently take the pager
// down with it.
func renderSlackConfig(a Alerting) string {
	recv := fmt.Sprintf(`    - name: %s
      slackConfigs:
        - sendResolved: true
          channel: %q
          # The SAME Secret apl-core mounts for its own Slack receivers, so one
          # OpenBao seed (and one rotation) serves both paths.
          apiURL:
            name: %s
            key: %s
`, AlertingReceiverSlack, "#"+a.slackChannel(), AplAlertmanagerCredentialsSecret, AplAlertmanagerCredentialsSlackKey)
	return renderAlertmanagerConfig(AlertingSlackConfigName,
		"Slack severities apl-core has no route for. critical is NOT here: apl-core's own\n"+
			"# critical route posts it, and a second match would post it twice. Nor are the\n"+
			"# alerts apl-core routes to Slack by name ("+strings.Join(aplCoreSlackOwned, ", ")+").",
		[]amcRoute{{
			receiver: AlertingReceiverSlack,
			matchers: [][3]string{
				{"severity", "=~", severityRegex(a.llzSlackSeverities())},
				{"alertname", "!~", strings.Join(aplCoreSlackOwned, "|")},
			},
		}}, recv)
}

// severityRegex anchors nothing on purpose: Alertmanager anchors every regex
// matcher itself (^(?:…)$), so "critical" cannot match "noncritical".
func severityRegex(sev []string) string {
	return strings.Join(sev, "|")
}
