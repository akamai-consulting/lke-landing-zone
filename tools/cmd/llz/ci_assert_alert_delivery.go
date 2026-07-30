package main

// ci_assert_alert_delivery.go implements `llz ci assert-alert-delivery` — the
// gate that a firing alert actually gets somewhere.
//
// The alerting stack has three links and CI covered only the first two:
//
//   rule syntax        promtool / check-prom-rules   — covered
//   rule LOADED        assert-scrape-targets          — covered
//   rule EVALUATES     alert-eval                     — report-only
//   alert DELIVERED    nothing                        — this gate
//
// The gap is real and total: Prometheus can evaluate a rule to FIRING and have
// nowhere to send it. If no Alertmanager is discovered — a dropped
// alertmanagerConfig, a NetworkPolicy between the two, a renamed Service — the
// alert fires into a void. Every dashboard shows it. Nobody is paged. There is no
// error anywhere, because from Prometheus's point of view firing an alert with no
// receivers is not a failure condition.
//
// SCOPE, stated honestly. This asserts the link LLZ owns: Prometheus →
// Alertmanager. It does NOT assert that Alertmanager forwards to a human
// destination (PagerDuty, Slack, email), because apl-core owns the receiver
// configuration and this repo ships none — a gate on receivers would fail on
// every cluster that has deliberately not configured one. What it proves is that
// a firing alert reaches something whose job is to route it, which is exactly the
// link that silently disappears.
//
// Two assertions:
//
//   1. Prometheus has at least one ACTIVE Alertmanager in /api/v1/alertmanagers.
//      Zero is the silent void above.
//   2. Alertmanager's own /api/v2/status answers, and its /api/v2/alerts is
//      readable — it is up and accepting the API Prometheus posts to.
//
// It deliberately does NOT require any alert to BE firing. A healthy cluster may
// have none, and a gate that demanded one would either flake or push someone to
// keep a permanent fake alert firing.
//
// FAIL-CLOSED: no active Alertmanagers, an unreachable Alertmanager, or an
// unparseable response all fail. Read-only.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// defaultAlertmanagerSpec is apl-core's Alertmanager Service.
const defaultAlertmanagerSpec = "monitoring/alertmanager-operated:9093"

func ciAssertAlertDeliveryCmd() *cobra.Command {
	var prom, am string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-alert-delivery",
		Short: "fail unless Prometheus has a live Alertmanager to deliver firing alerts to",
		Long: "Asserts the one alerting link nothing else covers: Prometheus → Alertmanager.\n" +
			"promtool checks rule syntax, assert-scrape-targets proves rules are LOADED, and\n" +
			"alert-eval proves they EVALUATE — but a rule can evaluate to FIRING and have\n" +
			"nowhere to go. With no Alertmanager discovered (a dropped alertmanagerConfig, a\n" +
			"NetworkPolicy, a renamed Service) the alert fires into a void: dashboards show\n" +
			"it, nobody is paged, and nothing anywhere reports an error, because firing with\n" +
			"no receivers is not a failure condition for Prometheus.\n\n" +
			"Checks /api/v1/alertmanagers has at least one ACTIVE endpoint, and that\n" +
			"Alertmanager itself answers /api/v2/status and /api/v2/alerts.\n\n" +
			"Scope: this is the link this repo owns. It does NOT assert Alertmanager\n" +
			"forwards to a human destination — apl-core owns receivers and none ship here,\n" +
			"so gating on them would fail every cluster that deliberately has none.\n\n" +
			"Does not require any alert to be firing. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertAlertDelivery(prom, am,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&am, "alertmanager", defaultAlertmanagerSpec,
		"the Alertmanager Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// activeAlertmanagers returns the URLs of the ACTIVE Alertmanagers Prometheus has
// discovered, and the count it considers dropped. Pure.
//
// Dropped endpoints are surfaced but do not by themselves fail: a rolling
// Alertmanager pod legitimately appears as dropped mid-restart. Zero ACTIVE is
// the failure, because that is the state in which alerts go nowhere.
func activeAlertmanagers(raw []byte) (active []string, dropped int, err error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ActiveAlertmanagers []struct {
				URL string `json:"url"`
			} `json:"activeAlertmanagers"`
			DroppedAlertmanagers []struct {
				URL string `json:"url"`
			} `json:"droppedAlertmanagers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, 0, fmt.Errorf("decoding /api/v1/alertmanagers: %w", err)
	}
	if resp.Status != "" && resp.Status != "success" {
		detail := resp.Error
		if detail == "" {
			detail = "status=" + resp.Status
		}
		return nil, 0, fmt.Errorf("Prometheus returned an error: %s", detail)
	}
	for _, a := range resp.Data.ActiveAlertmanagers {
		active = append(active, a.URL)
	}
	return active, len(resp.Data.DroppedAlertmanagers), nil
}

// alertmanagerReady reports whether an /api/v2/status body looks like a live
// Alertmanager. Pure.
//
// It requires the cluster/uptime block rather than merely a 200: a proxy or an
// unrelated service answering on the port would give a 200 too, and this gate
// exists precisely to distinguish "something answered" from "the thing that
// routes alerts answered".
func alertmanagerReady(raw []byte) (string, error) {
	var st struct {
		Cluster *struct {
			Status string `json:"status"`
		} `json:"cluster"`
		Uptime      string `json:"uptime"`
		VersionInfo *struct {
			Version string `json:"version"`
		} `json:"versionInfo"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("decoding /api/v2/status: %w", err)
	}
	if st.Cluster == nil && st.VersionInfo == nil {
		return "", fmt.Errorf("/api/v2/status carried neither a cluster nor a versionInfo block — " +
			"something answered on this port, but it does not look like Alertmanager")
	}
	desc := "alertmanager"
	if st.VersionInfo != nil && st.VersionInfo.Version != "" {
		desc += " v" + st.VersionInfo.Version
	}
	if st.Cluster != nil && st.Cluster.Status != "" {
		desc += " (cluster " + st.Cluster.Status + ")"
	}
	return desc, nil
}

// alertDeliveryProbe is one poll's outcome.
type alertDeliveryProbe struct {
	Active   []string
	Dropped  int
	AMDesc   string
	AlertCnt int
	FailWhy  string
}

func (p alertDeliveryProbe) OK() bool { return p.FailWhy == "" }

// probeAlertDelivery evaluates both links over their own port-forwards.
func probeAlertDelivery(prom, am string) (alertDeliveryProbe, error) {
	var p alertDeliveryProbe

	if err := withPrometheus(prom, func(get func(string) ([]byte, error)) error {
		raw, err := get("/api/v1/alertmanagers")
		if err != nil {
			return err
		}
		active, dropped, perr := activeAlertmanagers(raw)
		if perr != nil {
			return perr
		}
		p.Active, p.Dropped = active, dropped
		return nil
	}); err != nil {
		return p, err
	}

	if len(p.Active) == 0 {
		p.FailWhy = "Prometheus has NO active Alertmanager — every firing alert goes nowhere. " +
			"Nothing reports this as an error: firing with no receivers is not a failure condition. " +
			"Check the Prometheus CR's alerting.alertmanagers config, the Alertmanager Service name, " +
			"and any NetworkPolicy between monitoring's Prometheus and Alertmanager pods"
		return p, nil
	}

	// Alertmanager's own API. A discovered-but-dead endpoint delivers no better
	// than an absent one, so discovery alone is not the assertion.
	err := withForwardedAPI(am, forwardedService{name: "Alertmanager", readyPath: "/-/ready"},
		func(get func(string) ([]byte, error)) error {
			statusRaw, serr := get("/api/v2/status")
			if serr != nil {
				return serr
			}
			desc, derr := alertmanagerReady(statusRaw)
			if derr != nil {
				return derr
			}
			p.AMDesc = desc
			alertsRaw, aerr := get("/api/v2/alerts")
			if aerr != nil {
				return aerr
			}
			var alerts []any
			if jerr := json.Unmarshal(alertsRaw, &alerts); jerr != nil {
				return fmt.Errorf("decoding /api/v2/alerts: %w", jerr)
			}
			p.AlertCnt = len(alerts)
			return nil
		})
	return p, err
}

func runCIAssertAlertDelivery(prom, am string, settle, interval time.Duration) error {
	fmt.Println("## Alert-delivery assertion (Prometheus → Alertmanager)")

	var last alertDeliveryProbe
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		p, err := probeAlertDelivery(prom, am)
		last, lastErr = p, err
		if err == nil && p.OK() {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if err != nil {
			fmt.Printf("attempt %d: %v — retrying in %s\n", attempt, err, interval)
		} else {
			fmt.Printf("attempt %d: no active Alertmanager yet — retrying in %s\n", attempt, interval)
		}
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::alert-delivery check could not complete within %s (%v)\n", settle, lastErr)
		return fmt.Errorf("alert-delivery check could not complete within %s: %w", settle, lastErr)
	}
	if !last.OK() {
		fmt.Println("FAIL: " + last.FailWhy)
		fmt.Fprintln(os.Stderr, "::error::firing alerts have nowhere to be delivered")
		return fmt.Errorf("no active Alertmanager — firing alerts go nowhere")
	}

	droppedNote := ""
	if last.Dropped > 0 {
		droppedNote = fmt.Sprintf(", %d dropped", last.Dropped)
	}
	fmt.Printf("OK: %d active Alertmanager(s) [%s]%s\n",
		len(last.Active), strings.Join(last.Active, ", "), droppedNote)
	fmt.Printf("OK: %s reachable; %d alert(s) currently in its queue\n", last.AMDesc, last.AlertCnt)
	fmt.Println("Firing alerts have a live destination.")
	return nil
}
