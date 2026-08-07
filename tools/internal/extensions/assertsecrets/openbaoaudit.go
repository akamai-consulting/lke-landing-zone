package assertsecrets

// ci_assert_openbao_audit.go implements `llz ci assert-openbao-audit` — the e2e
// gate that proves OpenBao's audit log is actually ARRIVING in Loki, by reading
// it back out of Loki.
//
// WHY THIS EXISTS. The audit pipeline shipped nowhere for its entire life and
// nothing noticed. The promtail sidecar pushed to
// `loki-gateway.llz-observability`, but apl-core runs Loki in `monitoring` and
// nothing ever creates that Service — so every audit record went to a name that
// does not resolve. The NetworkPolicy is why it stayed invisible:
// `observabilityNamespace` was `llz-observability` too, so the egress allow was
// correct in SHAPE and wrong in TARGET — a rule granting nothing, pointed at an
// empty namespace, looking complete in every review. Both sides agreed with each
// other and neither agreed with the cluster.
//
// Nothing in the existing gate set can see that. `converge` and `health` see a
// Running pod (promtail retries a dead DNS name forever and stays up).
// `assert-loki` proves Loki is bootstrapped, not that anything reaches it.
// `assert-scrape-targets` covers the metrics path, not the log path. A static
// guard cannot close it either: any URL and any NetworkPolicy are consistent with
// each other right up until you ask the cluster. The only check that could have
// caught this is the round trip — write from OpenBao, read from Loki — so that is
// what this does. Its static half (the gate's target must match what the chart
// ships to, and the netpol must allow that same namespace) lives in the unit test
// beside it, where it fails at PR time instead of at e2e time.
//
// WHAT PASSING MEANS. Fresh OpenBao audit records exist in Loki within the
// lookback window: the audit device is enabled and writable, the sidecar can read
// the file, DNS resolves the gateway, the NetworkPolicy admits the egress, and
// Loki ingested it. Any one of those breaking empties the window and fails the
// gate. Read-only; reaches Loki via an ephemeral kubectl port-forward.
//
// FRESHNESS IS THE ASSERTION. The window starts at now-lookback, so entries that
// stopped arriving an hour ago cannot pass it. There is always traffic to audit
// on a converged cluster — Prometheus scrapes `/v1/sys/metrics` and the kubelet
// probes `/v1/sys/health` on a seconds-scale cadence, and every one of those is
// an audited request — so an empty window means the pipeline, not a quiet
// cluster.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

// defaultAuditLokiService is the Loki gateway the OpenBao promtail sidecar ships
// to, as <namespace>/<service>:<port>. MUST stay in agreement with the chart's
// openbaoPromtail.lokiPushUrl and platform.networkPolicy.observabilityNamespace —
// TestAuditGateDefaultsMatchTheChart enforces exactly that, because a gate
// querying a different Loki than the shipper writes to is the same
// correct-in-shape/wrong-in-target failure this verb exists to catch.
const defaultAuditLokiService = "monitoring/loki-gateway:80"

// defaultAuditTenant is the Loki tenant the sidecar pushes under (its promtail
// `tenant_id`). Inert while apl-core's Loki is single-tenant; load-bearing the
// day it isn't. See loki_query.go.
const defaultAuditTenant = "platform"

// defaultAuditSelector is the LogQL stream selector for the audit stream. The
// labels come from the promtail scrape_config the chart renders
// (templates/openbao-promtail-config.yaml).
const defaultAuditSelector = `{app="openbao",component="audit"}`

// auditRecordRe recognises an OpenBao audit record.
//
// A SUBSTRING match, deliberately, not a JSON parse: promtail's `replace` stage
// redacts residual token/secret_id/client_token substrings, and that rewrite can
// leave the line no longer valid JSON. Requiring json.Unmarshal to succeed would
// fail the gate on lines the pipeline delivered perfectly well. `"type":
// "request"|"response"` survives redaction untouched (it matches nothing in the
// redaction pattern) and is present on every audit record.
var auditRecordRe = regexp.MustCompile(`"type"\s*:\s*"(request|response)"`)

// auditProbe is one poll's verdict over the returned streams.
type auditProbe struct {
	Streams  int               // label-sets that matched
	Entries  int               // log lines returned
	Records  int               // of those, lines that look like OpenBao audit records
	Newest   time.Time         // timestamp of the freshest entry (zero when none)
	Labels   map[string]string // labels of the stream carrying the freshest entry
	Anomaly  string            // first non-audit line, truncated (diagnostic)
	Selector string
}

// OK reports whether the round trip is proven: something arrived, and what
// arrived is audit data.
func (p auditProbe) OK() bool { return p.Entries > 0 && p.Records > 0 }

// evalAuditStreams reduces query_range streams to a verdict. Pure.
func evalAuditStreams(selector string, streams []assertobs.LokiStream) auditProbe {
	p := auditProbe{Selector: selector, Streams: len(streams)}
	for _, s := range streams {
		for _, e := range s.Entries {
			p.Entries++
			if auditRecordRe.MatchString(e.Line) {
				p.Records++
			} else if p.Anomaly == "" {
				p.Anomaly = harborauth.TruncateForError([]byte(e.Line))
			}
			if e.At.After(p.Newest) {
				p.Newest = e.At
				p.Labels = s.Labels
			}
		}
	}
	return p
}

// probeAuditStream opens one port-forward, runs the range query over
// [now-lookback, now], and computes a verdict. A transport or decode error is
// returned so the poll loop can retry it rather than read it as "nothing
// arrived".
func probeAuditStream(loki, tenant, selector string, limit int, lookback time.Duration, now time.Time) (auditProbe, error) {
	var p auditProbe
	err := assertobs.WithLoki(loki, tenant, func(get func(string) ([]byte, error)) error {
		raw, gerr := get(assertobs.LokiQueryRangePath(selector, now.Add(-lookback), now, limit))
		if gerr != nil {
			return gerr
		}
		streams, perr := assertobs.ParseLokiStreams(raw)
		if perr != nil {
			return perr
		}
		p = evalAuditStreams(selector, streams)
		return nil
	})
	return p, err
}

// formatAuditLabels renders a stream's labels deterministically for the OK line.
func formatAuditLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "(no labels)"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// auditFailureDiagnosis names the ways the round trip breaks, in the order worth
// checking them. The gate proves a negative — nothing arrived — and the operator
// still has to find out why, so it hands over the list rather than the verdict
// alone.
func auditFailureDiagnosis(loki, selector string, lookback time.Duration) string {
	return strings.Join([]string{
		fmt.Sprintf("no OpenBao audit records in Loki (%s) for %s over the last %s. In order:",
			loki, selector, lookback),
		"  1. TARGET — `openbaoPromtail.lokiPushUrl` in the llz-openbao-platform chart must name a " +
			"Service that EXISTS. It named loki-gateway.llz-observability, which never did; apl-core " +
			"runs Loki in `monitoring`. Check: kubectl -n monitoring get svc loki-gateway",
		"  2. EGRESS — `platform.networkPolicy.observabilityNamespace` must be the SAME namespace. " +
			"When it agreed with a wrong URL, the allow granted nothing while looking complete. " +
			"Check: kubectl -n llz-openbao get networkpolicy -o yaml",
		"  3. SHIPPER — the promtail sidecar: kubectl -n llz-openbao logs platform-openbao-0 -c promtail. " +
			"A push failing on DNS or a 4xx says so there; `permission denied` means the audit file " +
			"mode regressed (it must be 0640 for uid 10000 to tail it)",
		"  4. SOURCE — the audit device itself. No file, no stream: kubectl -n llz-openbao exec " +
			"platform-openbao-0 -c openbao -- ls -l /openbao/audit/audit.log",
		"  5. TENANT — if Loki ever gains auth_enabled, reads must carry the sidecar's promtail " +
			"tenant_id (--tenant, default " + defaultAuditTenant + ")",
	}, "\n")
}

// runCIAssertOpenbaoAudit returns nil when audit records are arriving and an
// error otherwise (cobra exits 1 on it). The ::error:: annotations stay as direct
// writes: GitHub parses an annotation only at the start of a line, and a returned
// error reaches stderr behind main.go's "llz: " prefix.
func runCIAssertOpenbaoAudit(loki, tenant, selector string, limit int, lookback, settle, interval time.Duration) error {
	fmt.Println("## OpenBao audit-log pipeline assertion")
	if strings.TrimSpace(selector) == "" {
		fmt.Fprintln(os.Stderr, "::error::empty --selector — refusing to pass vacuously")
		return fmt.Errorf("empty --selector — refusing to pass vacuously")
	}
	if lookback <= 0 {
		fmt.Fprintln(os.Stderr, "::error::--lookback must be positive — a zero window can never contain an entry")
		return fmt.Errorf("--lookback must be positive")
	}
	fmt.Printf("Loki %s, tenant %s, selector %s, lookback %s\n", loki, tenant, selector, lookback)

	var last auditProbe
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		p, err := probeAuditStream(loki, tenant, selector, limit, lookback, time.Now())
		last, lastErr = p, err
		if err == nil && p.OK() {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if err != nil {
			fmt.Printf("attempt %d: could not query Loki at %s (%v) — retrying in %s\n", attempt, loki, err, interval)
		} else {
			fmt.Printf("attempt %d: no audit records in the window yet — retrying in %s\n", attempt, interval)
		}
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::could not query Loki at %s within %s (%v)\n", loki, settle, lastErr)
		return fmt.Errorf("could not query Loki at %s within %s: %w", loki, settle, lastErr)
	}
	if last.Entries > 0 && last.Records == 0 {
		// Something is writing to this stream and it isn't the audit device — a
		// promtail __path__ pointed at the wrong file, say. Green on entry count
		// alone would have called that a working audit pipeline.
		fmt.Printf("FAIL: %d entries matched %s but none are OpenBao audit records — first: %s\n",
			last.Entries, selector, last.Anomaly)
		fmt.Fprintln(os.Stderr, "::error::the audit stream carries entries that are not OpenBao audit records")
		return fmt.Errorf("%s returned %d entries, none of them OpenBao audit records (first: %s)",
			selector, last.Entries, last.Anomaly)
	}
	if !last.OK() {
		fmt.Println("FAIL: " + auditFailureDiagnosis(loki, selector, lookback))
		fmt.Fprintln(os.Stderr, "::error::OpenBao audit records are not reaching Loki")
		return fmt.Errorf("no OpenBao audit records in Loki (%s) for %s within the last %s",
			loki, selector, lookback)
	}

	fmt.Printf("OK: %d audit record(s) in %d stream(s) within the last %s; freshest %s old %s\n",
		last.Records, last.Streams, lookback,
		time.Since(last.Newest).Truncate(time.Second), formatAuditLabels(last.Labels))
	fmt.Println("OpenBao audit records are reaching Loki.")
	return nil
}
