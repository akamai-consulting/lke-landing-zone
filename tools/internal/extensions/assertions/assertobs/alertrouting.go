package assertobs

// alertrouting.go implements `llz ci assert-alert-routing` — the gate that a
// firing alert reaches the receivers spec.alerting names, exactly once each,
// checked at the one place that decides it: Alertmanager.
//
// WHERE IT SITS. assert-alert-delivery proves Prometheus → Alertmanager. This
// proves the next hop: Alertmanager → the PagerDuty/Slack receiver, as far as
// Alertmanager's own decision goes. Two chains feed it, and every link of both
// fails silently:
//
//	slack:     spec.alerting → apl-overlay alerts.yaml → reconciler → apl-<env>
//	             env/settings/alerts.yaml (AplAlertSet) → apl-core receivers
//	           + alerting.yaml ExternalSecret ← OpenBao alerts/webhooks
//	             → apl-secrets/alerts-secrets → apl-core's alertmanager-credentials
//	pagerduty: spec.alerting → alerting.yaml (Argo) → ExternalSecret ← OpenBao
//	             → AlertmanagerConfig accepted → merged into the running config
//	             → routes match every namespace (matcher strategy, apl-overlay)
//
// A failure at any link leaves a cluster where every manifest is valid, every
// Application is Synced, Alertmanager is up, and nobody is notified.
//
// FOUR ASSERTIONS, ALL AT THE CONSUMER, plus one report:
//
//  1. RECEIVERS LOADED. /api/v2/status carries the config Alertmanager is
//     actually running. Each receiver the spec implies must be in it, with its
//     integration and its credential field set (the API masks secrets as
//     "<secret>", so "set" is observable without the value). For Slack that
//     includes apl-core's global slack_api_url_file — and, because the API
//     cannot show a FILE's contents, the Secret behind it: apl-core's
//     alertmanager-credentials must carry a non-empty slackUrl.
//  2. ROUTING, EXACTLY ONCE. The gate creates a silence, posts synthetic probe
//     alerts the silence matches, and reads back the receivers Alertmanager
//     itself computed for each one (/api/v2/alerts reports them, silenced or
//     not). For every probe it COUNTS the receivers carrying each integration
//     and requires the spec's number — one, or zero. Counting is what catches
//     a duplicate: LLZ's and apl-core's Slack routes both matching one alert
//     pass "reaches Slack" and post twice. Probes carry a non-monitoring
//     namespace and one carries none (the matcher-strategy regression), and an
//     info probe must notify nobody.
//  3. NO TEAM ALERTMANAGER CARRIES LLZ'S RECEIVERS. apl-core's per-team
//     Alertmanagers select every AlertmanagerConfig too, and the platform
//     Prometheus sends them every alert, so one that merged LLZ's CRs pages a
//     second time for namespace=monitoring alerts (clusterspec/alerting.go).
//     Each non-platform Alertmanager's running config is read; zero of them is
//     a clean answer, an unreadable one is a failure.
//  4. (report only) DELIVERY. Alertmanager's own
//     alertmanager_notifications_failed_total, per integration, from the same
//     forward. Report-only: the release e2e points every receiver at a
//     `.invalid` endpoint on purpose, and a gate that failed on that would be
//     switched off. LLZAlertNotificationsFailing (observability component) is
//     the alerting half.
//
// THE EXPECTED SET COMES FROM THE SPEC, never from the cluster under test
// (clusterspec.AlertRoutingProbes / AlertingExpectedReceivers): a gate that
// listed what Alertmanager has and checked it against itself would pass on an
// empty config. Only the CLASSIFICATION of a receiver (which integrations it
// carries) is read from the loaded config — the number required is the spec's.
//
// IT NEVER NOTIFIES. The silence is created FIRST, verified `active`, and only
// then are the probes posted, all over one port-forward so they land on the
// replica that holds the silence. Alertmanager's notify pipeline checks silences
// at send time, and a resolved notification is only sent for an alert whose
// firing one was — so a probe that was silenced throughout produces no
// notification at all. The silence outlives the probes and is left to expire on
// its own: expiring it while a probe is still active is the one way this gate
// could page someone.
//
// WHAT IT CANNOT SEE, stated rather than implied. It proves Alertmanager would
// hand the alert to the integration with a key set. It does not prove the key is
// valid, that the provider accepted the event, or that egress reaches it — that
// is assertion 4's report and docs/alerting.md's "fire one real alert" step.
//
// NOT APPLICABLE vs FAILED. A spec with no receiver for this env — none
// declared, opted out, or an env without observability — is reported, loudly,
// and exits 0: failing every instance that has not configured paging would get
// the step switched off. Everything else — an unreadable spec, an unknown env,
// an unreachable Alertmanager, an unparseable response — is a failure, because
// "could not tell" is not "nothing wrong".

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	yaml "gopkg.in/yaml.v3"
)

// alertRoutingProbeName is the alertname every probe carries, and what the
// silence matches on (together with the per-run id).
const alertRoutingProbeName = "LLZAlertRoutingProbe"

// Probe label keys. llz_probe scopes the silence and the read-back to THIS run;
// llz_probe_case says which probe an alert is.
const (
	probeIDLabel   = "llz_probe"
	probeCaseLabel = "llz_probe_case"
)

// probeLifetime bounds how long a probe alert stays active, and silenceLifetime
// how long the silence covering it lasts. The silence must comfortably outlive
// the probes (see the header), so it is several times longer.
const (
	probeLifetime   = 2 * time.Minute
	silenceLifetime = 30 * time.Minute
)

// withAlertmanager opens one port-forward to an Alertmanager for a whole probe.
// A package var so tests can replace the transport.
var withAlertmanager = func(spec string, fn func(do httpDo) error) error {
	return withForwardedHTTP(spec, forwardedService{name: "Alertmanager", readyPath: "/-/ready"}, fn)
}

// alertingTarget is the env the gate grades and what its spec says.
type alertingTarget struct {
	Env      string
	Alerting clusterspec.Alerting // effective (instance merged with the env's override)
	Applies  bool                 // false: the env deploys no observability, so it is skipped
}

// loadAlertingSpec reads the instance checkout and resolves env's effective
// spec.alerting. A seam so the not-applicable and unreadable arms are testable.
//
// An empty env is accepted only when the instance has exactly one: a gate that
// guessed which deployment it was pointed at would grade one env's cluster
// against another's spec.
var loadAlertingSpec = func(env string) (alertingTarget, error) {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return alertingTarget{}, err
	}
	if env == "" {
		names := lz.EnvNames()
		if len(names) != 1 {
			return alertingTarget{}, fmt.Errorf("--region (or $REGION) is required: the instance has %d environments %v", len(names), names)
		}
		env = names[0]
	}
	e, ok := lz.Spec.Environments[env]
	if !ok {
		return alertingTarget{}, fmt.Errorf("environment %q is not in this instance (have %v)", env, lz.EnvNames())
	}
	return alertingTarget{Env: env, Alerting: lz.AlertingFor(env), Applies: clusterspec.AlertingApplies(e)}, nil
}

// secretKeySet reports whether ns/name carries a non-empty data[key]. ok=false
// means the cluster could not be read — "could not tell", not "absent". The
// value is never printed; only its presence is. A seam for tests.
var secretKeySet = func(ns, name, key string) (set, ok bool) {
	v, ok := kubectlprobe.JSONPathOK("-n", ns, "get", "secret", name, "-o", "jsonpath={.data."+key+"}")
	return v != "", ok
}

// listAlertmanagers returns every Alertmanager CR as namespace/name. ok=false
// means the list could not be read. A seam for tests.
var listAlertmanagers = func() (refs []string, ok bool) {
	type am struct {
		Metadata struct{ Name, Namespace string } `json:"metadata"`
	}
	items, ok := kubectlprobe.ListOK[am]("get", "alertmanagers.monitoring.coreos.com", "-A")
	if !ok {
		return nil, false
	}
	for _, i := range items {
		refs = append(refs, i.Metadata.Namespace+"/"+i.Metadata.Name)
	}
	sort.Strings(refs)
	return refs, true
}

var (
	alertRoutingNow   = time.Now
	alertRoutingSleep = time.Sleep
	newProbeID        = func() string {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		return hex.EncodeToString(b)
	}
)

// loadedReceiver is what Alertmanager's running config says about one receiver.
type loadedReceiver struct {
	PagerDutyConfigs, PagerDutyKeysSet int
	SlackConfigs, SlackURLsSet         int
}

// integrations returns how many configs of each integration this receiver has.
func (r loadedReceiver) integrations() map[string]int {
	return map[string]int{clusterspec.IntegrationPagerDuty: r.PagerDutyConfigs, clusterspec.IntegrationSlack: r.SlackConfigs}
}

// loadedConfig is the parsed running config.
type loadedConfig struct {
	Receivers map[string]loadedReceiver
	// SlackGlobalURLFile is global.slack_api_url_file — apl-core's Slack
	// receivers carry no api_url of their own and inherit this.
	SlackGlobalURLFile string
}

// parseLoadedConfig parses /api/v2/status. Pure.
//
// An empty receivers list is returned as an ERROR: every Alertmanager config has
// at least the root route's receiver, so zero means this was not the document we
// think it is.
func parseLoadedConfig(statusJSON []byte) (loadedConfig, error) {
	var st struct {
		Config *struct {
			Original string `json:"original"`
		} `json:"config"`
	}
	if err := json.Unmarshal(statusJSON, &st); err != nil {
		return loadedConfig{}, fmt.Errorf("decoding /api/v2/status: %w", err)
	}
	if st.Config == nil || strings.TrimSpace(st.Config.Original) == "" {
		return loadedConfig{}, fmt.Errorf("/api/v2/status carried no config.original — cannot see what Alertmanager is running")
	}
	var cfg struct {
		Global struct {
			SlackAPIURL     string `yaml:"slack_api_url"`
			SlackAPIURLFile string `yaml:"slack_api_url_file"`
		} `yaml:"global"`
		Receivers []struct {
			Name             string `yaml:"name"`
			PagerdutyConfigs []struct {
				RoutingKey     string `yaml:"routing_key"`
				RoutingKeyFile string `yaml:"routing_key_file"`
				ServiceKey     string `yaml:"service_key"`
			} `yaml:"pagerduty_configs"`
			SlackConfigs []struct {
				APIURL     string `yaml:"api_url"`
				APIURLFile string `yaml:"api_url_file"`
			} `yaml:"slack_configs"`
		} `yaml:"receivers"`
	}
	if err := yaml.Unmarshal([]byte(st.Config.Original), &cfg); err != nil {
		return loadedConfig{}, fmt.Errorf("parsing Alertmanager's loaded config: %w", err)
	}
	if len(cfg.Receivers) == 0 {
		return loadedConfig{}, fmt.Errorf("the loaded Alertmanager config declares no receivers at all — not a config this gate understands")
	}
	globalSlack := cfg.Global.SlackAPIURL != "" || cfg.Global.SlackAPIURLFile != ""
	out := loadedConfig{Receivers: make(map[string]loadedReceiver, len(cfg.Receivers)), SlackGlobalURLFile: cfg.Global.SlackAPIURLFile}
	for _, r := range cfg.Receivers {
		var lr loadedReceiver
		for _, p := range r.PagerdutyConfigs {
			lr.PagerDutyConfigs++
			if p.RoutingKey != "" || p.RoutingKeyFile != "" || p.ServiceKey != "" {
				lr.PagerDutyKeysSet++
			}
		}
		for _, s := range r.SlackConfigs {
			lr.SlackConfigs++
			if s.APIURL != "" || s.APIURLFile != "" || globalSlack {
				lr.SlackURLsSet++
			}
		}
		out.Receivers[r.Name] = lr
	}
	return out, nil
}

// gradeLoadedReceivers checks every expected receiver is loaded with its
// integration and credential field set. One problem per receiver that is not.
// Pure.
func gradeLoadedReceivers(loaded loadedConfig, expected []clusterspec.ExpectedReceiver) []string {
	var problems []string
	for _, e := range expected {
		r, ok := loaded.Receivers[e.Name]
		if !ok {
			problems = append(problems, fmt.Sprintf("receiver %s is not in Alertmanager's running config", e.Name))
			continue
		}
		switch e.Integration {
		case clusterspec.IntegrationPagerDuty:
			if r.PagerDutyConfigs == 0 {
				problems = append(problems, fmt.Sprintf("receiver %s is loaded with no pagerduty_configs", e.Name))
			} else if r.PagerDutyKeysSet < r.PagerDutyConfigs {
				problems = append(problems, fmt.Sprintf("receiver %s has a pagerduty_config with no routing_key", e.Name))
			}
		case clusterspec.IntegrationSlack:
			if r.SlackConfigs == 0 {
				problems = append(problems, fmt.Sprintf("receiver %s is loaded with no slack_configs — apl-core's alert "+
					"receivers do not include slack, so the AplAlertSet has not reached apl-core", e.Name))
			} else if r.SlackURLsSet < r.SlackConfigs {
				problems = append(problems, fmt.Sprintf("receiver %s has a slack_config with no api_url and there is no global slack_api_url", e.Name))
			}
		}
	}
	return problems
}

// gradeSlackCredential checks the Secret behind apl-core's slack_api_url_file.
// The status API shows the PATH, not the file, so a config that names a missing
// or empty key looks identical to a working one there. Pure over the seam's
// answer.
func gradeSlackCredential(set, ok bool) string {
	ns, name, key := clusterspec.AlertmanagerNamespace, clusterspec.AplAlertmanagerCredentialsSecret, clusterspec.AplAlertmanagerCredentialsSlackKey
	switch {
	case !ok:
		return fmt.Sprintf("could not read Secret %s/%s — cannot tell whether Slack has a webhook URL", ns, name)
	case !set:
		return fmt.Sprintf("Secret %s/%s has no %s — apl-core's Slack receivers would send to an empty webhook "+
			"(check %s/%s, which LLZ's ExternalSecret writes from OpenBao %s)", ns, name, key,
			clusterspec.AplSecretsNamespace, clusterspec.AplAlertsSecretName, clusterspec.SlackOpenBaoPath)
	}
	return ""
}

// silenceBody is the POST /api/v2/silences payload covering this run's probes.
func silenceBody(id string, now time.Time) ([]byte, error) {
	type matcher struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		IsRegex bool   `json:"isRegex"`
		IsEqual bool   `json:"isEqual"`
	}
	return json.Marshal(map[string]any{
		"matchers": []matcher{
			{Name: "alertname", Value: alertRoutingProbeName, IsEqual: true},
			{Name: probeIDLabel, Value: id, IsEqual: true},
		},
		// A minute in the past so clock skew between this runner and the pod can
		// never leave the silence `pending` when the probes arrive.
		"startsAt":  now.Add(-time.Minute).UTC().Format(time.RFC3339),
		"endsAt":    now.Add(silenceLifetime).UTC().Format(time.RFC3339),
		"createdBy": "llz ci assert-alert-routing",
		"comment": "Covers the routing gate's synthetic probe alerts so they never notify. " +
			"Left to expire on its own: expiring it while a probe is active would page.",
	})
}

// probeAlertsBody is the POST /api/v2/alerts payload: one alert per probe.
func probeAlertsBody(probes []clusterspec.AlertRoutingProbe, id string, now time.Time) ([]byte, error) {
	type alert struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt"`
		EndsAt      string            `json:"endsAt"`
	}
	out := make([]alert, 0, len(probes))
	for i, p := range probes {
		labels := map[string]string{
			"alertname":    alertRoutingProbeName,
			probeIDLabel:   id,
			probeCaseLabel: strconv.Itoa(i),
		}
		for k, v := range p.Labels {
			labels[k] = v
		}
		out = append(out, alert{
			Labels: labels,
			Annotations: map[string]string{
				"summary": "Synthetic probe from `llz ci assert-alert-routing` — silenced, never notifies. Safe to ignore.",
			},
			StartsAt: now.UTC().Format(time.RFC3339),
			EndsAt:   now.Add(probeLifetime).UTC().Format(time.RFC3339),
		})
	}
	return json.Marshal(out)
}

// probeVerdict is one probe's outcome.
type probeVerdict struct {
	Probe     clusterspec.AlertRoutingProbe
	Receivers []string
	Counts    map[string]int // integration → receivers carrying it
	Missing   []string       // MustReach receivers it did not reach
	Seen      bool
	Silenced  bool
	OK        bool
}

// gradeProbeRouting reads /api/v2/alerts (filtered to this run) and decides each
// probe against the loaded config's receiver classification. Pure. A probe
// Alertmanager never reports is not OK — it was not examined, and a gate that
// passed it would be passing on nothing. A receiver the alert reached that the
// loaded config does not declare is an error: it cannot be classified, and
// counting it as zero would launder a duplicate.
func gradeProbeRouting(alertsJSON []byte, probes []clusterspec.AlertRoutingProbe, id string, loaded loadedConfig) ([]probeVerdict, error) {
	var alerts []struct {
		Labels    map[string]string `json:"labels"`
		Receivers []struct {
			Name string `json:"name"`
		} `json:"receivers"`
		Status struct {
			SilencedBy []string `json:"silencedBy"`
		} `json:"status"`
	}
	if err := json.Unmarshal(alertsJSON, &alerts); err != nil {
		return nil, fmt.Errorf("decoding /api/v2/alerts: %w", err)
	}
	byCase := map[string]int{}
	for i, a := range alerts {
		if a.Labels[probeIDLabel] != id || a.Labels["alertname"] != alertRoutingProbeName {
			continue
		}
		byCase[a.Labels[probeCaseLabel]] = i
	}
	out := make([]probeVerdict, 0, len(probes))
	for i, p := range probes {
		v := probeVerdict{Probe: p, Counts: map[string]int{}}
		idx, ok := byCase[strconv.Itoa(i)]
		if !ok {
			out = append(out, v)
			continue
		}
		a := alerts[idx]
		v.Seen = true
		v.Silenced = len(a.Status.SilencedBy) > 0
		for _, r := range a.Receivers {
			v.Receivers = append(v.Receivers, r.Name)
		}
		sort.Strings(v.Receivers)
		for _, r := range v.Receivers {
			lr, known := loaded.Receivers[r]
			if !known {
				return nil, fmt.Errorf("probe %q reached receiver %q, which the loaded config does not declare — the status read and the alerts read disagree", p.Name, r)
			}
			for integ, n := range lr.integrations() {
				if n > 0 {
					v.Counts[integ]++
				}
			}
		}
		for _, want := range p.MustReach {
			if !slices.Contains(v.Receivers, want) {
				v.Missing = append(v.Missing, want)
			}
		}
		v.OK = len(v.Missing) == 0
		for integ, want := range p.Integrations {
			if v.Counts[integ] != want {
				v.OK = false
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// describe renders a verdict's failure as one line.
func (v probeVerdict) describe() string {
	var parts []string
	if len(v.Missing) > 0 {
		parts = append(parts, "did not reach "+strings.Join(v.Missing, ", "))
	}
	for _, integ := range []string{clusterspec.IntegrationPagerDuty, clusterspec.IntegrationSlack} {
		if want := v.Probe.Integrations[integ]; v.Counts[integ] != want {
			parts = append(parts, fmt.Sprintf("%d %s path(s), want exactly %d", v.Counts[integ], integ, want))
		}
	}
	return strings.Join(parts, "; ") + " — routed to [" + strings.Join(v.Receivers, ", ") + "]"
}

// alertRoutingResult is one run's evidence.
type alertRoutingResult struct {
	LoadedNames     []string
	ReceiverProblem []string
	Verdicts        []probeVerdict
	ProbeErr        error
	Delivery        []deliveryStat
	DeliveryErr     error
}

// misrouted counts probes that were not decided as the spec declares (an unseen
// probe counts: it was not examined).
func (r alertRoutingResult) misrouted() int {
	n := 0
	for _, v := range r.Verdicts {
		if !v.OK {
			n++
		}
	}
	return n
}

// done reports whether this attempt is a pass.
func (r alertRoutingResult) done() bool {
	return len(r.ReceiverProblem) == 0 && r.ProbeErr == nil && len(r.Verdicts) > 0 && r.misrouted() == 0
}

// probeAlertRouting does the platform-Alertmanager half over ONE forward.
// When the receivers are not loaded yet it returns without probing; routing is
// decided only once there is a config to route by.
//
// BOTH STATES ARE RETRIED within the settle window, routing included. On a fresh
// cluster several clocks run in series — the reconciler's push of alerts.yaml,
// apl-operator applying it, ESO's refresh, the operator's reconcile, the
// config-reloader, and the apl-overlay's matcher strategy reaching the
// Alertmanager CR — so a partly-wired config is briefly a legitimate in-between
// state. Each retry uses a fresh probe id under a fresh silence, so earlier
// probes cannot stand in for later ones.
func probeAlertRouting(amSpec string, a clusterspec.Alerting) (alertRoutingResult, error) {
	var res alertRoutingResult
	err := withAlertmanager(amSpec, func(do httpDo) error {
		statusRaw, err := do("GET", "/api/v2/status", nil)
		if err != nil {
			return err
		}
		loaded, err := parseLoadedConfig(statusRaw)
		if err != nil {
			return err
		}
		for name := range loaded.Receivers {
			res.LoadedNames = append(res.LoadedNames, name)
		}
		sort.Strings(res.LoadedNames)
		res.ReceiverProblem = gradeLoadedReceivers(loaded, clusterspec.AlertingExpectedReceivers(a))
		if a.HasReceiver("slack") {
			if p := gradeSlackCredential(secretKeySet(clusterspec.AlertmanagerNamespace,
				clusterspec.AplAlertmanagerCredentialsSecret, clusterspec.AplAlertmanagerCredentialsSlackKey)); p != "" {
				res.ReceiverProblem = append(res.ReceiverProblem, p)
			}
		}
		if len(res.ReceiverProblem) > 0 {
			return nil
		}
		res.Verdicts, res.ProbeErr = runRoutingProbes(do, clusterspec.AlertRoutingProbes(a), loaded)
		if metrics, err := do("GET", "/metrics", nil); err != nil {
			res.DeliveryErr = err
		} else {
			res.Delivery, res.DeliveryErr = parseNotificationStats(metrics)
		}
		return nil
	})
	return res, err
}

// runRoutingProbes is the silence → verify → post → read-back sequence.
func runRoutingProbes(do httpDo, probes []clusterspec.AlertRoutingProbe, loaded loadedConfig) ([]probeVerdict, error) {
	if len(probes) == 0 {
		return nil, fmt.Errorf("spec.alerting yields no routing probes — nothing would be examined")
	}
	id := newProbeID()
	now := alertRoutingNow()

	body, err := silenceBody(id, now)
	if err != nil {
		return nil, err
	}
	raw, err := do("POST", "/api/v2/silences", body)
	if err != nil {
		return nil, fmt.Errorf("creating the probe silence (no probe was posted): %w", err)
	}
	var created struct {
		SilenceID string `json:"silenceID"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.SilenceID == "" {
		return nil, fmt.Errorf("creating the probe silence returned no silenceID (no probe was posted): %s", string(raw))
	}
	// Verify BEFORE posting. A probe posted under a silence that is not active is
	// a probe that can page — so if this cannot be confirmed, nothing is posted.
	raw, err = do("GET", "/api/v2/silence/"+url.PathEscape(created.SilenceID), nil)
	if err != nil {
		return nil, fmt.Errorf("reading back the probe silence (no probe was posted): %w", err)
	}
	var sil struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &sil); err != nil || sil.Status.State != "active" {
		return nil, fmt.Errorf("the probe silence %s is %q, not active — refusing to post probes that could page", created.SilenceID, sil.Status.State)
	}

	body, err = probeAlertsBody(probes, id, now)
	if err != nil {
		return nil, err
	}
	if _, err := do("POST", "/api/v2/alerts", body); err != nil {
		return nil, fmt.Errorf("posting probe alerts: %w", err)
	}

	filter := url.QueryEscape(fmt.Sprintf(`%s="%s"`, probeIDLabel, id))
	var verdicts []probeVerdict
	// Alertmanager stores a posted alert synchronously, so this normally answers
	// on the first read; the short retry covers a slow API, not a settle window.
	for attempt := 0; attempt < 5; attempt++ {
		raw, err = do("GET", "/api/v2/alerts?filter="+filter, nil)
		if err != nil {
			return nil, err
		}
		verdicts, err = gradeProbeRouting(raw, probes, id, loaded)
		if err != nil {
			return nil, err
		}
		allSeen := true
		for _, v := range verdicts {
			allSeen = allSeen && v.Seen
		}
		if allSeen {
			break
		}
		alertRoutingSleep(2 * time.Second)
	}
	return verdicts, nil
}

// teamAlertmanagerFinding is one non-platform Alertmanager's verdict.
type teamAlertmanagerFinding struct {
	Ref     string   // namespace/name
	LLZ     []string // LLZ receivers its running config carries
	ReadErr error
}

// checkTeamAlertmanagers reads every non-platform Alertmanager's running config
// and reports the LLZ receivers in each. ok=false: the list itself could not be
// read. Zero team Alertmanagers is an answer, not vacuity — the platform
// Alertmanager was examined above; this is the "no second path" half.
func checkTeamAlertmanagers() (findings []teamAlertmanagerFinding, ok bool) {
	refs, ok := listAlertmanagers()
	if !ok {
		return nil, false
	}
	for _, ref := range refs {
		ns, _, _ := strings.Cut(ref, "/")
		if ns == clusterspec.AlertmanagerNamespace {
			continue // the platform Alertmanager — graded above
		}
		f := teamAlertmanagerFinding{Ref: ref}
		f.ReadErr = withAlertmanager(ns+"/alertmanager-operated:9093", func(do httpDo) error {
			raw, err := do("GET", "/api/v2/status", nil)
			if err != nil {
				return err
			}
			loaded, err := parseLoadedConfig(raw)
			if err != nil {
				return err
			}
			f.LLZ = llzReceiversIn(loaded)
			return nil
		})
		findings = append(findings, f)
	}
	return findings, true
}

// llzReceiversIn lists the receivers in a loaded config that came from LLZ's
// AlertmanagerConfigs. Pure.
func llzReceiversIn(loaded loadedConfig) []string {
	var out []string
	for name := range loaded.Receivers {
		if clusterspec.IsLLZAlertingReceiver(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// deliveryStat is one integration's notification counters on the platform
// Alertmanager since its process started.
type deliveryStat struct {
	Integration   string
	Total, Failed float64
}

// parseNotificationStats extracts alertmanager_notifications_total and
// alertmanager_notifications_failed_total for the integrations LLZ wires, summed
// over every other label (receiver_name, reason). Pure. No series at all for an
// integration the spec selects is reported as zero sent — a fact, not an error:
// the counters only gain series once a notification is attempted.
func parseNotificationStats(metrics []byte) ([]deliveryStat, error) {
	stats := map[string]*deliveryStat{}
	for _, integ := range []string{clusterspec.IntegrationPagerDuty, clusterspec.IntegrationSlack} {
		stats[integ] = &deliveryStat{Integration: integ}
	}
	sawFamily := false
	sc := bufio.NewScanner(bytes.NewReader(metrics))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		var failed bool
		switch {
		case strings.HasPrefix(line, "alertmanager_notifications_failed_total{"):
			failed = true
		case strings.HasPrefix(line, "alertmanager_notifications_total{"):
		case strings.HasPrefix(line, "# TYPE alertmanager_notifications_total "):
			sawFamily = true
			continue
		default:
			continue
		}
		open, closeIdx := strings.IndexByte(line, '{'), strings.LastIndexByte(line, '}')
		if open < 0 || closeIdx < open {
			continue
		}
		integ := labelValue(line[open+1:closeIdx], "integration")
		st, ok := stats[integ]
		if !ok {
			continue
		}
		fields := strings.Fields(line[closeIdx+1:])
		if len(fields) == 0 {
			return nil, fmt.Errorf("parsing %q: no sample value", line)
		}
		val, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("parsing %q: %w", line, err)
		}
		if failed {
			st.Failed += val
		} else {
			st.Total += val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawFamily {
		return nil, fmt.Errorf("/metrics carried no alertmanager_notifications_total family — not Alertmanager's metrics endpoint")
	}
	return []deliveryStat{*stats[clusterspec.IntegrationPagerDuty], *stats[clusterspec.IntegrationSlack]}, nil
}

// labelValue returns one label's value from a Prometheus text-format label set.
// Alertmanager's label values here are plain identifiers, so no escape handling.
func labelValue(set, name string) string {
	for _, kv := range strings.Split(set, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && k == name {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// alertRoutingRemedies is what to check, in pipeline order, when the receivers
// are not loaded. A bare "receiver missing" sends the reader to a cluster to
// find out which of several things broke.
func alertRoutingRemedies(env string, a clusterspec.Alerting) []string {
	ns := clusterspec.AlertmanagerNamespace
	var out []string
	n := 0
	step := func(s string) { n++; out = append(out, fmt.Sprintf("%d. %s", n, s)) }
	step("rendered: apl-values/" + env + "/apps/observability/kustomization.yaml lists " + clusterspec.AlertingFile +
		" and the llz-observability Application is Synced")
	if a.HasReceiver("pagerduty") {
		step(fmt.Sprintf("seeded:   llz openbao set %s %s=<integration key>", clusterspec.PagerDutyOpenBaoPath, clusterspec.PagerDutyOpenBaoProperty))
		step(fmt.Sprintf("synced:   kubectl -n %s get externalsecret %s  (Ready=True; SecretSyncedError means the key above is missing)", ns, clusterspec.AlertingConfigName))
		step(fmt.Sprintf("accepted: kubectl -n %s get alertmanagerconfig %s -o yaml, and the operator's log — it REJECTS a CR whose Secret or key is absent", ns, clusterspec.AlertingConfigName))
	}
	if a.HasReceiver("slack") {
		step(fmt.Sprintf("seeded:   llz openbao set %s %s=<webhook url>", clusterspec.SlackOpenBaoPath, clusterspec.SlackOpenBaoProperty))
		step(fmt.Sprintf("synced:   kubectl -n %s get externalsecret %s, then kubectl -n %s get externalsecret %s (apl-core's; both Ready=True)",
			clusterspec.AplSecretsNamespace, clusterspec.AplAlertsSecretName, ns, clusterspec.AplAlertmanagerCredentialsSecret))
		step("overlaid: env/settings/alerts.yaml on the apl-" + env + " branch lists receivers: [slack] — the apl-overlay reconciler " +
			"writes it (llz_apl_overlay_alerts_target_unusable=1 says why it could not); apl-operator then re-renders the platform config")
	}
	step("reloaded: Alertmanager's config-reloader picks a new config up within a minute or two of it being written")
	return out
}

func runCIAssertAlertRouting(env, amSpec string, settle, interval time.Duration) error {
	fmt.Println("## Alert-routing assertion (Alertmanager → spec.alerting receivers)")

	tgt, err := loadAlertingSpec(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::could not resolve spec.alerting (%v) — this is 'could not tell', not 'nothing to check'\n", err)
		return fmt.Errorf("read spec.alerting: %w", err)
	}
	a := tgt.Alerting
	switch {
	case !tgt.Applies:
		fmt.Printf("NOT APPLICABLE: %s does not deploy observability, so it runs no Alertmanager and spec.alerting is skipped for it.\n", tgt.Env)
		return nil
	case !a.Active():
		fmt.Printf("NOT APPLICABLE: spec.alerting declares no receiver for %s (unset, [none], or opted out in environments/%s.yaml).\n", tgt.Env, tgt.Env)
		fmt.Println("Every alert this cluster fires aggregates in Alertmanager and NOTIFIES NOBODY. See docs/alerting.md.")
		fmt.Fprintf(os.Stderr, "::warning::spec.alerting declares no receiver for %s — alerts on this cluster notify nobody\n", tgt.Env)
		return nil
	}
	fmt.Printf("%s: spec.alerting.receivers %v (pagerduty severities %v, slack severities %v)\n", tgt.Env, a.Receivers,
		pick(a.HasReceiver("pagerduty"), a.PagerDutySeverities()), pick(a.HasReceiver("slack"), a.SlackSeverities()))

	var last alertRoutingResult
	var lastErr error
	deadline := alertRoutingNow().Add(settle)
	for attempt := 1; ; attempt++ {
		last, lastErr = probeAlertRouting(amSpec, a)
		if lastErr == nil && last.done() {
			break
		}
		if alertRoutingNow().After(deadline) {
			break
		}
		switch {
		case lastErr != nil:
			fmt.Printf("attempt %d: %v — retrying in %s\n", attempt, lastErr, interval)
		case len(last.ReceiverProblem) > 0:
			fmt.Printf("attempt %d: %s — retrying in %s\n", attempt, strings.Join(last.ReceiverProblem, "; "), interval)
		case last.ProbeErr != nil:
			fmt.Printf("attempt %d: probe: %v — retrying in %s\n", attempt, last.ProbeErr, interval)
		default:
			fmt.Printf("attempt %d: %d of %d probe(s) misrouted — retrying in %s\n", attempt, last.misrouted(), len(last.Verdicts), interval)
		}
		alertRoutingSleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::alert-routing check could not complete within %s (%v)\n", settle, lastErr)
		return fmt.Errorf("alert-routing check could not complete within %s: %w", settle, lastErr)
	}
	if len(last.ReceiverProblem) > 0 {
		for _, p := range last.ReceiverProblem {
			fmt.Println("FAIL: " + p)
		}
		fmt.Printf("Receivers Alertmanager IS running: %s\n", strings.Join(last.LoadedNames, ", "))
		fmt.Println("Check, in pipeline order:")
		for _, r := range alertRoutingRemedies(tgt.Env, a) {
			fmt.Println("  " + r)
		}
		fmt.Fprintln(os.Stderr, "::error::spec.alerting's receivers are not loaded in Alertmanager — alerts notify nobody")
		return fmt.Errorf("spec.alerting receivers not loaded after %s", settle)
	}
	var names []string
	for _, e := range clusterspec.AlertingExpectedReceivers(a) {
		names = append(names, e.Name)
	}
	fmt.Printf("OK: receivers loaded with their keys set: %s\n", strings.Join(names, ", "))

	if last.ProbeErr != nil {
		fmt.Fprintf(os.Stderr, "::error::alert-routing probe failed: %v\n", last.ProbeErr)
		return fmt.Errorf("alert-routing probe: %w", last.ProbeErr)
	}
	failed := 0
	for _, v := range last.Verdicts {
		switch {
		case !v.Seen:
			failed++
			fmt.Printf("FAIL: %s — Alertmanager never reported the probe alert, so nothing was examined\n", v.Probe.Name)
		case !v.OK:
			failed++
			fmt.Printf("FAIL: %s — %s\n", v.Probe.Name, v.describe())
		default:
			fmt.Printf("OK:   %s — pagerduty×%d slack×%d\n", v.Probe.Name, v.Counts[clusterspec.IntegrationPagerDuty], v.Counts[clusterspec.IntegrationSlack])
		}
		if v.Seen && !v.Silenced {
			// The silence was verified active before posting, so this is the
			// marker lagging, not a page — but say so rather than hide it.
			fmt.Fprintf(os.Stderr, "::warning::probe %q was not yet marked silenced when read back\n", v.Probe.Name)
		}
	}
	if failed > 0 {
		fmt.Println("A probe that should route and does not is usually the matcher strategy: the operator adds")
		fmt.Printf("namespace=%q to LLZ's AlertmanagerConfig routes unless the Alertmanager CR carries\n", clusterspec.AlertmanagerNamespace)
		fmt.Printf("alertmanagerConfigMatcherStrategy.type=%s (apl-overlay apps.alertmanager._rawValues). Check:\n", clusterspec.AlertmanagerConfigMatcherStrategy)
		fmt.Printf("  kubectl -n %s get alertmanager -o jsonpath='{.items[*].spec.alertmanagerConfigMatcherStrategy}'\n", clusterspec.AlertmanagerNamespace)
		fmt.Println("A probe with TWO paths to one integration is a duplicate notification: a receiver turned on")
		fmt.Println("outside the spec (the Console's alert settings, a hand-written AlertmanagerConfig) or apl-core")
		fmt.Println("routing a severity LLZ also routes.")
		fmt.Fprintf(os.Stderr, "::error::%d of %d alert-routing probe(s) were routed wrongly\n", failed, len(last.Verdicts))
		return fmt.Errorf("%d of %d alert-routing probes failed", failed, len(last.Verdicts))
	}
	fmt.Printf("All %d routing probes decided as spec.alerting declares, each on exactly the paths it names.\n", len(last.Verdicts))

	if err := reportTeamAlertmanagers(); err != nil {
		return err
	}
	reportDelivery(last)
	return nil
}

// reportTeamAlertmanagers is assertion 3. It FAILS on a team Alertmanager that
// carries LLZ's receivers: that is a second page for every namespace=monitoring
// alert, and nothing else in the cluster says so.
func reportTeamAlertmanagers() error {
	findings, ok := checkTeamAlertmanagers()
	if !ok {
		fmt.Fprintln(os.Stderr, "::error::could not list Alertmanagers — cannot tell whether a team Alertmanager pages a second time")
		return fmt.Errorf("could not list Alertmanagers")
	}
	if len(findings) == 0 {
		fmt.Println("OK: no team Alertmanager runs on this cluster, so LLZ's receivers have no second path.")
		return nil
	}
	bad := 0
	for _, f := range findings {
		switch {
		case f.ReadErr != nil:
			bad++
			fmt.Printf("FAIL: team Alertmanager %s — could not read its running config (%v)\n", f.Ref, f.ReadErr)
		case len(f.LLZ) > 0:
			bad++
			fmt.Printf("FAIL: team Alertmanager %s carries LLZ's receivers %v — it pages a SECOND time for every namespace=%s alert\n",
				f.Ref, f.LLZ, clusterspec.AlertmanagerNamespace)
		default:
			fmt.Printf("OK:   team Alertmanager %s carries none of LLZ's receivers\n", f.Ref)
		}
	}
	if bad > 0 {
		fmt.Println("apl-core's team Alertmanagers select every AlertmanagerConfig in every namespace, and LLZ has no")
		fmt.Println("channel into a team Alertmanager's selectors. Turn the team's managed Alertmanager off")
		fmt.Println("(managedMonitoring.alertmanager: false — the App Platform Console's team settings) until apl-core")
		fmt.Println("scopes them; docs/upstream-asks.md §4 tracks the ask.")
		fmt.Fprintf(os.Stderr, "::error::%d team Alertmanager(s) would duplicate LLZ's notifications, or could not be read\n", bad)
		return fmt.Errorf("%d team Alertmanager(s) carry LLZ's receivers or are unreadable", bad)
	}
	return nil
}

// reportDelivery is assertion 4 — REPORT ONLY (see the header).
func reportDelivery(r alertRoutingResult) {
	if r.DeliveryErr != nil {
		fmt.Fprintf(os.Stderr, "::warning::could not read Alertmanager's notification counters (%v) — delivery is unreported\n", r.DeliveryErr)
		return
	}
	fmt.Println("Delivery (report only — since Alertmanager started; LLZAlertNotificationsFailing alerts on it):")
	for _, s := range r.Delivery {
		fmt.Printf("  %-9s sent %.0f, failed %.0f\n", s.Integration, s.Total, s.Failed)
		if s.Failed > 0 {
			fmt.Fprintf(os.Stderr, "::warning::Alertmanager has failed %.0f %s notification(s) — check the key, the endpoint and egress\n", s.Failed, s.Integration)
		}
	}
}

func pick(ok bool, v []string) []string {
	if !ok {
		return nil
	}
	return v
}
