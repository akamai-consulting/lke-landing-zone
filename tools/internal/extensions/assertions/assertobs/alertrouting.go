package assertobs

// alertrouting.go implements `llz ci assert-alert-routing` — the gate that a
// firing alert reaches the receiver spec.alerting names, checked at the one
// place that decides it: Alertmanager.
//
// WHERE IT SITS. assert-alert-delivery proves Prometheus → Alertmanager. This
// proves the next hop LLZ now owns: Alertmanager → the PagerDuty/Slack receiver,
// as far as Alertmanager's own decision goes. The chain it covers is long, and
// every link fails silently:
//
//	spec.alerting → llz render → apps/observability/alerting.yaml (Argo)
//	  → ExternalSecret ← OpenBao alerts/*          (unseeded key: no Secret)
//	  → AlertmanagerConfig accepted by the operator (no Secret: CR rejected)
//	  → merged into Alertmanager's config           (receiver loaded)
//	  → routes match alerts from EVERY namespace    (matcher strategy, apl-overlay)
//
// A failure at any link leaves a cluster where every manifest is valid, every
// Application is Synced, Alertmanager is up, and nobody is paged.
//
// TWO ASSERTIONS, BOTH AT THE CONSUMER:
//
//  1. RECEIVERS LOADED. /api/v2/status carries the config Alertmanager is
//     actually running. Each receiver the spec implies must be in it, under the
//     name the operator gives it, with its routing key / webhook set (the API
//     masks secrets as "<secret>", so "set" is observable without the value).
//  2. ROUTING. The gate creates a silence, posts synthetic probe alerts that
//     the silence matches, and reads back the receivers Alertmanager itself
//     computed for each one (/api/v2/alerts reports them, silenced or not). The
//     probes carry a namespace that is NOT `monitoring`, and one carries none —
//     under the operator's default matcher strategy neither would route, which
//     is the regression a config read cannot see. A must-NOT-route probe pins
//     the exclusion, so "pages on everything" fails too.
//
// THE EXPECTED SET COMES FROM THE SPEC, never from the cluster under test
// (clusterspec.AlertRoutingProbes / AlertingExpectedReceivers): a gate that
// listed what Alertmanager has and checked it against itself would pass on an
// empty config.
//
// IT NEVER PAGES. The silence is created FIRST, verified `active`, and only then
// are the probes posted, all over one port-forward so they land on the replica
// that holds the silence. Alertmanager's notify pipeline checks silences at send
// time, and a resolved notification is only sent for an alert whose firing one
// was — so a probe that was silenced throughout produces no notification at all.
// The silence outlives the probes (which end after a couple of minutes) and is
// left to expire on its own: expiring it while a probe is still active is the
// one way this gate could page someone.
//
// WHAT IT CANNOT SEE, stated rather than implied. It proves Alertmanager would
// hand the alert to the PagerDuty integration with a key set. It does not prove
// the key is valid, that PagerDuty accepted the event, or that egress from the
// Alertmanager pod reaches PagerDuty — Alertmanager's own
// alertmanager_notifications_failed_total{integration="pagerduty"} is where that
// shows, and the runbook in docs/alerting.md says to fire a real test alert once
// after seeding.
//
// NOT APPLICABLE vs FAILED. A spec with no paging receiver is reported, loudly,
// as "pages nobody" and exits 0: failing every instance that has not configured
// paging would get the step switched off. Everything else — an unreadable spec,
// an unreachable Alertmanager, an unparseable response — is a failure, because
// "could not tell" is not "nothing wrong".

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
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

// withAlertmanager opens one port-forward to Alertmanager for a whole probe.
// A package var so tests can replace the transport.
var withAlertmanager = func(spec string, fn func(do httpDo) error) error {
	return withForwardedHTTP(spec, forwardedService{name: "Alertmanager", readyPath: "/-/ready"}, fn)
}

// loadAlertingSpec reads spec.alerting from the instance checkout. A seam so
// the not-applicable and unreadable arms are testable.
var loadAlertingSpec = func() (clusterspec.Alerting, error) {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return clusterspec.Alerting{}, err
	}
	return lz.Spec.Alerting, nil
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

// loadedReceivers parses /api/v2/status and returns the receivers in the config
// Alertmanager is running, keyed by name. Pure.
//
// An empty receivers list is returned as an ERROR: every Alertmanager config has
// at least the root route's receiver, so zero means this was not the document we
// think it is.
func loadedReceivers(statusJSON []byte) (map[string]loadedReceiver, error) {
	var st struct {
		Config *struct {
			Original string `json:"original"`
		} `json:"config"`
	}
	if err := json.Unmarshal(statusJSON, &st); err != nil {
		return nil, fmt.Errorf("decoding /api/v2/status: %w", err)
	}
	if st.Config == nil || strings.TrimSpace(st.Config.Original) == "" {
		return nil, fmt.Errorf("/api/v2/status carried no config.original — cannot see what Alertmanager is running")
	}
	var cfg struct {
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
		return nil, fmt.Errorf("parsing Alertmanager's loaded config: %w", err)
	}
	if len(cfg.Receivers) == 0 {
		return nil, fmt.Errorf("the loaded Alertmanager config declares no receivers at all — not a config this gate understands")
	}
	out := make(map[string]loadedReceiver, len(cfg.Receivers))
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
			if s.APIURL != "" || s.APIURLFile != "" {
				lr.SlackURLsSet++
			}
		}
		out[r.Name] = lr
	}
	return out, nil
}

// gradeLoadedReceivers checks every expected receiver is loaded with its secret
// field set. Returns one problem per receiver that is not. Pure.
func gradeLoadedReceivers(loaded map[string]loadedReceiver, expected map[string]string) []string {
	var problems []string
	for _, name := range sortedMapKeys(expected) {
		r, ok := loaded[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("receiver %s is not in Alertmanager's running config", name))
			continue
		}
		switch {
		case strings.HasPrefix(expected[name], "pagerduty_configs"):
			if r.PagerDutyConfigs == 0 {
				problems = append(problems, fmt.Sprintf("receiver %s is loaded with no pagerduty_configs", name))
			} else if r.PagerDutyKeysSet < r.PagerDutyConfigs {
				problems = append(problems, fmt.Sprintf("receiver %s has a pagerduty_config with no routing_key", name))
			}
		case strings.HasPrefix(expected[name], "slack_configs"):
			if r.SlackConfigs == 0 {
				problems = append(problems, fmt.Sprintf("receiver %s is loaded with no slack_configs", name))
			} else if r.SlackURLsSet < r.SlackConfigs {
				problems = append(problems, fmt.Sprintf("receiver %s has a slack_config with no api_url", name))
			}
		}
	}
	return problems
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
	Seen      bool
	Silenced  bool
	OK        bool
}

// gradeProbeRouting reads /api/v2/alerts (filtered to this run) and decides each
// probe. Pure. A probe Alertmanager never reports is not OK — it was not
// examined, and a gate that passed it would be passing on nothing.
func gradeProbeRouting(alertsJSON []byte, probes []clusterspec.AlertRoutingProbe, id string) ([]probeVerdict, error) {
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
		v := probeVerdict{Probe: p}
		if idx, ok := byCase[strconv.Itoa(i)]; ok {
			a := alerts[idx]
			v.Seen = true
			v.Silenced = len(a.Status.SilencedBy) > 0
			for _, r := range a.Receivers {
				v.Receivers = append(v.Receivers, r.Name)
			}
			sort.Strings(v.Receivers)
			routed := false
			for _, r := range v.Receivers {
				if r == p.Receiver {
					routed = true
				}
			}
			v.OK = routed == p.MustRoute
		}
		out = append(out, v)
	}
	return out, nil
}

// alertRoutingResult is one run's evidence.
type alertRoutingResult struct {
	LoadedNames     []string
	ReceiverProblem []string
	Verdicts        []probeVerdict
	ProbeErr        error
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

// probeAlertRouting does the whole consumer-side check over ONE forward.
// When the receivers are not loaded yet it returns without probing; routing is
// decided only once there is a config to route by.
//
// BOTH STATES ARE RETRIED within the settle window, routing included. On a fresh
// cluster four clocks run in series — ESO's refresh, the operator's reconcile,
// the config-reloader, and the apl-overlay's matcher strategy reaching the
// Alertmanager CR through apl-operator — and the last can trail the first three,
// so a namespace-scoped route is briefly a legitimate in-between state. Each
// retry uses a fresh probe id under a fresh silence, so earlier probes cannot
// stand in for later ones.
func probeAlertRouting(amSpec string, a clusterspec.Alerting) (alertRoutingResult, error) {
	var res alertRoutingResult
	err := withAlertmanager(amSpec, func(do httpDo) error {
		statusRaw, err := do("GET", "/api/v2/status", nil)
		if err != nil {
			return err
		}
		loaded, err := loadedReceivers(statusRaw)
		if err != nil {
			return err
		}
		for name := range loaded {
			res.LoadedNames = append(res.LoadedNames, name)
		}
		sort.Strings(res.LoadedNames)
		res.ReceiverProblem = gradeLoadedReceivers(loaded, clusterspec.AlertingExpectedReceivers(a))
		if len(res.ReceiverProblem) > 0 {
			return nil
		}
		res.Verdicts, res.ProbeErr = runRoutingProbes(do, clusterspec.AlertRoutingProbes(a))
		return nil
	})
	return res, err
}

// runRoutingProbes is the silence → verify → post → read-back sequence.
func runRoutingProbes(do httpDo, probes []clusterspec.AlertRoutingProbe) ([]probeVerdict, error) {
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
		verdicts, err = gradeProbeRouting(raw, probes, id)
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

// alertRoutingRemedies is what to check, in pipeline order, when the receivers
// are not loaded. A bare "receiver missing" sends the reader to a cluster to
// find out which of five things broke.
func alertRoutingRemedies(a clusterspec.Alerting) []string {
	ns, name := clusterspec.AlertmanagerNamespace, clusterspec.AlertingConfigName
	var seeds []string
	if a.HasReceiver("pagerduty") {
		seeds = append(seeds, fmt.Sprintf("llz openbao set %s %s=<integration key>", clusterspec.PagerDutyOpenBaoPath, clusterspec.PagerDutyOpenBaoProperty))
	}
	if a.HasReceiver("slack") {
		seeds = append(seeds, fmt.Sprintf("llz openbao set %s %s=<webhook url>", clusterspec.SlackOpenBaoPath, clusterspec.SlackOpenBaoProperty))
	}
	return []string{
		"1. rendered: apl-values/<env>/apps/observability/kustomization.yaml lists " + clusterspec.AlertingFile + " and the llz-observability Application is Synced",
		"2. seeded:   " + strings.Join(seeds, " ; "),
		fmt.Sprintf("3. synced:   kubectl -n %s get externalsecret %s  (Ready=True; SecretSyncedError means the key above is missing)", ns, name),
		fmt.Sprintf("4. accepted: kubectl -n %s get alertmanagerconfig %s -o yaml, and the operator's log — it REJECTS a CR whose Secret or key is absent", ns, name),
		"5. reloaded: Alertmanager's config-reloader picks the merged config up within a minute or two of the operator writing it",
	}
}

func runCIAssertAlertRouting(amSpec string, settle, interval time.Duration) error {
	fmt.Println("## Alert-routing assertion (Alertmanager → spec.alerting receivers)")

	a, err := loadAlertingSpec()
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::could not read spec.alerting (%v) — this is 'could not tell', not 'nothing to check'\n", err)
		return fmt.Errorf("read spec.alerting: %w", err)
	}
	if !a.Active() {
		fmt.Println("NOT APPLICABLE: spec.alerting declares no receiver (receivers unset or [none]).")
		fmt.Println("Every alert this cluster fires aggregates in Alertmanager and PAGES NOBODY. See docs/alerting.md.")
		fmt.Fprintln(os.Stderr, "::warning::spec.alerting declares no receiver — alerts on this cluster page nobody")
		return nil
	}
	fmt.Printf("spec.alerting.receivers: %v\n", a.Receivers)

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
		for _, r := range alertRoutingRemedies(a) {
			fmt.Println("  " + r)
		}
		fmt.Fprintln(os.Stderr, "::error::spec.alerting's receivers are not loaded in Alertmanager — alerts page nobody")
		return fmt.Errorf("spec.alerting receivers not loaded after %s", settle)
	}
	fmt.Printf("OK: receivers loaded with their keys set: %s\n", strings.Join(sortedMapKeys(clusterspec.AlertingExpectedReceivers(a)), ", "))

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
			fmt.Printf("FAIL: %s — Alertmanager routed it to [%s]\n", v.Probe.Name, strings.Join(v.Receivers, ", "))
		default:
			fmt.Printf("OK:   %s\n", v.Probe.Name)
		}
		if v.Seen && !v.Silenced {
			// The silence was verified active before posting, so this is the
			// marker lagging, not a page — but say so rather than hide it.
			fmt.Fprintf(os.Stderr, "::warning::probe %q was not yet marked silenced when read back\n", v.Probe.Name)
		}
	}
	if failed > 0 {
		fmt.Println("A probe that should route and does not is almost always the matcher strategy: the operator adds")
		fmt.Printf("namespace=%q to the CR's route unless the Alertmanager CR carries\n", clusterspec.AlertmanagerNamespace)
		fmt.Printf("alertmanagerConfigMatcherStrategy.type=%s (apl-overlay apps.alertmanager._rawValues). Check:\n", clusterspec.AlertmanagerConfigMatcherStrategy)
		fmt.Printf("  kubectl -n %s get alertmanager -o jsonpath='{.items[*].spec.alertmanagerConfigMatcherStrategy}'\n", clusterspec.AlertmanagerNamespace)
		fmt.Fprintf(os.Stderr, "::error::%d of %d alert-routing probe(s) were routed wrongly — alerts may page nobody\n", failed, len(last.Verdicts))
		return fmt.Errorf("%d of %d alert-routing probes failed", failed, len(last.Verdicts))
	}
	fmt.Printf("All %d routing probes decided as spec.alerting declares. (Delivery to the provider itself is not\n", len(last.Verdicts))
	fmt.Println("checked here — see alertmanager_notifications_failed_total and docs/alerting.md.)")
	return nil
}
