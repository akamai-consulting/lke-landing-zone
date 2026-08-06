package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertobs"
	"gopkg.in/yaml.v3"
)

// lokiPushReply renders a query_range response carrying one audit-shaped line per
// offset (seconds back from now).
func lokiPushReply(labels map[string]string, lines []string, now time.Time) []byte {
	var vals []string
	for i, l := range lines {
		ts := now.Add(-time.Duration(i) * time.Second).UnixNano()
		vals = append(vals, fmt.Sprintf(`["%d",%q]`, ts, l))
	}
	var lbl []string
	for k, v := range labels {
		lbl = append(lbl, fmt.Sprintf("%q:%q", k, v))
	}
	return []byte(fmt.Sprintf(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{%s},"values":[%s]}]}}`,
		strings.Join(lbl, ","), strings.Join(vals, ",")))
}

const sampleAuditLine = `{"time":"2026-07-30T12:00:00Z","type":"response","auth":{"display_name":"eso-pusher"},` +
	`"request":{"operation":"read","path":"secret/data/obj/platform"}}`

// seamLoki makes withLoki answer from reply/err instead of opening a
// port-forward, and records what was asked.
func seamLoki(t *testing.T, reply func(apiPath string) ([]byte, error)) *[]string {
	t.Helper()
	var asked []string
	orig := assertobs.WithLoki
	t.Cleanup(func() { assertobs.WithLoki = orig })
	assertobs.WithLoki = func(_, _ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(p string) ([]byte, error) {
			asked = append(asked, p)
			return reply(p)
		})
	}
	return &asked
}

func TestLokiQueryRangePath(t *testing.T) {
	start := time.Unix(1700000000, 0)
	end := start.Add(30 * time.Minute)
	got := assertobs.LokiQueryRangePath(`{app="openbao",component="audit"}`, start, end, 20)

	base, rawQuery, ok := strings.Cut(got, "?")
	if !ok || base != "/loki/api/v1/query_range" {
		t.Fatalf("unexpected path: %s", got)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("query is not parseable: %v", err)
	}
	if q.Get("query") != `{app="openbao",component="audit"}` {
		t.Errorf("selector not round-tripped: %q", q.Get("query"))
	}
	// Nanosecond epochs — a seconds-epoch start would silently widen the window
	// by a factor of 1e9 and make the freshness assertion meaningless.
	if q.Get("start") != "1700000000000000000" {
		t.Errorf("start should be a nanosecond epoch, got %q", q.Get("start"))
	}
	if q.Get("end") != fmt.Sprint(end.UnixNano()) {
		t.Errorf("end should be a nanosecond epoch, got %q", q.Get("end"))
	}
	if q.Get("limit") != "20" || q.Get("direction") != "backward" {
		t.Errorf("limit/direction wrong: %q %q", q.Get("limit"), q.Get("direction"))
	}
}

func TestParseLokiStreams(t *testing.T) {
	now := time.Unix(1700000000, 0)
	streams, err := assertobs.ParseLokiStreams(lokiPushReply(
		map[string]string{"app": "openbao", "component": "audit"},
		[]string{sampleAuditLine, sampleAuditLine}, now))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(streams) != 1 || len(streams[0].Entries) != 2 {
		t.Fatalf("expected 1 stream with 2 entries, got %+v", streams)
	}
	if streams[0].Labels["component"] != "audit" {
		t.Errorf("labels not parsed: %v", streams[0].Labels)
	}
	if !streams[0].Entries[0].At.Equal(now) {
		t.Errorf("timestamp not parsed: %s", streams[0].Entries[0].At)
	}

	// Undecodable input must be an ERROR, never an empty result: "nothing
	// arrived" and "we could not tell" are different answers, and collapsing
	// them is how the gate would pass vacuously.
	for name, raw := range map[string]string{
		"not json":        `<html>502 Bad Gateway</html>`,
		"error envelope":  `{"status":"error","error":"parse error"}`,
		"metric result":   `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		"bad timestamp":   `{"data":{"resultType":"streams","result":[{"stream":{},"values":[["not-a-ts","x"]]}]}}`,
		"values too thin": `{"data":{"resultType":"streams","result":[{"stream":{},"values":[["1700000000000000000"]]}]}}`,
	} {
		got, err := assertobs.ParseLokiStreams([]byte(raw))
		if name == "values too thin" {
			// A short pair is skipped, not fatal — but it must not become an entry.
			if err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			} else if len(got) != 1 || len(got[0].Entries) != 0 {
				t.Errorf("%s: malformed value should yield no entries, got %+v", name, got)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected an error, got %+v", name, got)
		}
	}
}

func TestEvalAuditStreams(t *testing.T) {
	now := time.Unix(1700000000, 0)
	older := assertobs.LokiStream{
		Labels:  map[string]string{"pod": "platform-openbao-1"},
		Entries: []assertobs.LokiEntry{{At: now.Add(-time.Hour), Line: sampleAuditLine}},
	}
	newer := assertobs.LokiStream{
		Labels: map[string]string{"pod": "platform-openbao-0"},
		Entries: []assertobs.LokiEntry{
			{At: now, Line: sampleAuditLine},
			{At: now.Add(-time.Minute), Line: "not an audit record at all"},
		},
	}
	p := evalAuditStreams(defaultAuditSelector, []assertobs.LokiStream{older, newer})
	if p.Streams != 2 || p.Entries != 3 || p.Records != 2 {
		t.Fatalf("miscounted: %+v", p)
	}
	if !p.Newest.Equal(now) {
		t.Errorf("newest should be the freshest entry across streams, got %s", p.Newest)
	}
	if p.Labels["pod"] != "platform-openbao-0" {
		t.Errorf("labels should come from the stream carrying the freshest entry: %v", p.Labels)
	}
	if p.Anomaly == "" {
		t.Error("the non-audit line should be captured as a diagnostic")
	}
	if !p.OK() {
		t.Error("a stream with audit records is OK")
	}
	if (auditProbe{}).OK() {
		t.Error("an empty probe must never be OK")
	}
	// Entries that are not audit records must not carry the verdict on their own.
	only := evalAuditStreams(defaultAuditSelector, []assertobs.LokiStream{{
		Entries: []assertobs.LokiEntry{{At: now, Line: "some other workload's log line"}},
	}})
	if only.OK() {
		t.Error("entries that are not audit records must not pass the gate")
	}
}

// The redaction stage in the promtail pipeline can leave a line that is no
// longer valid JSON, which is why the record check is a substring match. Assert
// the recognizer survives it — a json.Unmarshal-based check would not.
func TestAuditRecordRecognizerSurvivesRedaction(t *testing.T) {
	redacted := `{"type":"request","auth":{"client_token=REDACTED,"display_name":"eso-pusher"}}`
	if !auditRecordRe.MatchString(redacted) {
		t.Error("a redacted (JSON-invalid) audit line must still be recognised")
	}
	if auditRecordRe.MatchString(`{"level":"info","msg":"type: request"}`) {
		t.Error("prose mentioning a request must not count as an audit record")
	}
}

func TestRunCIAssertOpenbaoAuditPasses(t *testing.T) {
	asked := seamLoki(t, func(string) ([]byte, error) {
		return lokiPushReply(map[string]string{"app": "openbao"}, []string{sampleAuditLine}, time.Now()), nil
	})
	if err := runCIAssertOpenbaoAudit(defaultAuditLokiService, defaultAuditTenant, defaultAuditSelector,
		20, 30*time.Minute, 0, time.Millisecond); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "query_range") {
		t.Errorf("expected one query_range call, got %v", *asked)
	}
}

func TestRunCIAssertOpenbaoAuditFailsOnEmptyWindow(t *testing.T) {
	// The regression this gate exists for: promtail shipping to a Service that
	// does not exist leaves the stream empty, while every pod stays Running.
	seamLoki(t, func(string) ([]byte, error) {
		return []byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`), nil
	})
	err := runCIAssertOpenbaoAudit(defaultAuditLokiService, defaultAuditTenant, defaultAuditSelector,
		20, 30*time.Minute, 0, time.Millisecond)
	if err == nil {
		t.Fatal("an empty window must fail the gate")
	}
	if !strings.Contains(err.Error(), "no OpenBao audit records") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestRunCIAssertOpenbaoAuditFailsOnNonAuditEntries(t *testing.T) {
	seamLoki(t, func(string) ([]byte, error) {
		return lokiPushReply(map[string]string{"app": "openbao"},
			[]string{"promtail tailed the wrong file"}, time.Now()), nil
	})
	err := runCIAssertOpenbaoAudit(defaultAuditLokiService, defaultAuditTenant, defaultAuditSelector,
		20, 30*time.Minute, 0, time.Millisecond)
	if err == nil {
		t.Fatal("entries that are not audit records must fail the gate")
	}
	if !strings.Contains(err.Error(), "none of them OpenBao audit records") {
		t.Errorf("the failure should name what was wrong: %v", err)
	}
}

func TestRunCIAssertOpenbaoAuditFailsOnUnreachableLoki(t *testing.T) {
	seamLoki(t, func(string) ([]byte, error) { return nil, errors.New("connection refused") })
	err := runCIAssertOpenbaoAudit(defaultAuditLokiService, defaultAuditTenant, defaultAuditSelector,
		20, 30*time.Minute, 0, time.Millisecond)
	if err == nil {
		t.Fatal("an unreachable Loki must fail the gate, not pass it as an empty window")
	}
	if !strings.Contains(err.Error(), "could not query Loki") {
		t.Errorf("a transport failure must be reported as one: %v", err)
	}
}

func TestRunCIAssertOpenbaoAuditRetriesWithinSettle(t *testing.T) {
	n := 0
	seamLoki(t, func(string) ([]byte, error) {
		n++
		if n < 3 {
			return []byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`), nil
		}
		return lokiPushReply(map[string]string{"app": "openbao"}, []string{sampleAuditLine}, time.Now()), nil
	})
	if err := runCIAssertOpenbaoAudit(defaultAuditLokiService, defaultAuditTenant, defaultAuditSelector,
		20, 30*time.Minute, 2*time.Second, time.Millisecond); err != nil {
		t.Fatalf("a late-arriving stream inside the settle budget must pass: %v", err)
	}
	if n < 3 {
		t.Errorf("expected the poll loop to retry, saw %d attempts", n)
	}
}

func TestRunCIAssertOpenbaoAuditRefusesVacuousArguments(t *testing.T) {
	// Both would otherwise be a color.Green gate that asserted nothing.
	seamLoki(t, func(string) ([]byte, error) {
		t.Error("Loki must not be queried at all")
		return nil, nil
	})
	if err := runCIAssertOpenbaoAudit(defaultAuditLokiService, defaultAuditTenant, "  ",
		20, 30*time.Minute, 0, time.Millisecond); err == nil {
		t.Error("an empty selector must fail")
	}
	if err := runCIAssertOpenbaoAudit(defaultAuditLokiService, defaultAuditTenant, defaultAuditSelector,
		20, 0, 0, time.Millisecond); err == nil {
		t.Error("a zero lookback window must fail")
	}
}

// ── the static half ──────────────────────────────────────────────────────────
//
// The live gate proves the round trip on a cluster. This proves, at PR time, that
// the gate is aimed at the place the chart actually ships to — and that the
// NetworkPolicy allows that same namespace. That pair diverging silently, while
// both halves looked internally consistent, is the entire bug: an egress rule
// correct in shape and wrong in target grants nothing and reviews clean.

// auditShippingValues is the slice of the llz-openbao-platform values that
// decides where audit records go and whether they are allowed to get there.
type auditShippingValues struct {
	Platform struct {
		NetworkPolicy struct {
			ObservabilityNamespace string `yaml:"observabilityNamespace"`
		} `yaml:"networkPolicy"`
	} `yaml:"platform"`
	OpenbaoPromtail struct {
		Enabled     bool   `yaml:"enabled"`
		LokiPushURL string `yaml:"lokiPushUrl"`
	} `yaml:"openbaoPromtail"`
}

func loadAuditShippingValues(t *testing.T) auditShippingValues {
	t.Helper()
	var v auditShippingValues
	if err := yaml.Unmarshal([]byte(rawOpenBaoValues(t)), &v); err != nil {
		t.Fatalf("parsing %s: %v", openbaoChartValues, err)
	}
	return v
}

// splitPushURL returns the namespace, service and port a promtail push URL names.
func splitPushURL(t *testing.T, raw string) (ns, svc, port string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("lokiPushUrl is not a URL: %v", err)
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) < 2 {
		t.Fatalf("lokiPushUrl host %q is not <service>.<namespace>[.svc…]", u.Hostname())
	}
	port = u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return parts[1], parts[0], port
}

func TestAuditGateDefaultsMatchTheChart(t *testing.T) {
	v := loadAuditShippingValues(t)
	if !v.OpenbaoPromtail.Enabled {
		t.Fatal("openbaoPromtail is disabled in the chart — this gate would assert a pipeline nobody ships")
	}
	ns, svc, port := splitPushURL(t, v.OpenbaoPromtail.LokiPushURL)

	// 1. The gate must query the Loki the sidecar pushes to.
	if want := fmt.Sprintf("%s/%s:%s", ns, svc, port); defaultAuditLokiService != want {
		t.Errorf("defaultAuditLokiService is %q but the chart ships to %q (%s)",
			defaultAuditLokiService, want, v.OpenbaoPromtail.LokiPushURL)
	}
	// 2. The NetworkPolicy must open egress to that same namespace. This is the
	//    original defect verbatim: it was llz-observability on both sides, so the
	//    allow was internally consistent and granted nothing.
	if got := v.Platform.NetworkPolicy.ObservabilityNamespace; got != ns {
		t.Errorf("networkPolicy.observabilityNamespace is %q but lokiPushUrl targets namespace %q — "+
			"the egress allow describes traffic that does not exist", got, ns)
	}
}

func TestAuditGateSelectorAndTenantMatchThePromtailConfig(t *testing.T) {
	cfg := readForTLSTest(t, repoRootForTLSTest(t),
		"kubernetes-charts/llz-openbao-platform/templates/openbao-promtail-config.yaml")

	// Every matcher in the gate's selector must be a label the sidecar actually
	// attaches; otherwise the query returns nothing on a perfectly healthy
	// pipeline and the gate fails for its own reasons.
	matchers := regexp.MustCompile(`(\w+)="([^"]*)"`).FindAllStringSubmatch(defaultAuditSelector, -1)
	if len(matchers) == 0 {
		t.Fatalf("defaultAuditSelector %q has no matchers — it would select every stream", defaultAuditSelector)
	}
	for _, m := range matchers {
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(m[1]) + `:\s*` + regexp.QuoteMeta(m[2]) + `\s*$`).MatchString(cfg) {
			t.Errorf("selector matches %s=%q, but the promtail scrape_config attaches no such label", m[1], m[2])
		}
	}
	if !regexp.MustCompile(`(?m)^\s*tenant_id:\s*` + regexp.QuoteMeta(defaultAuditTenant) + `\s*$`).MatchString(cfg) {
		t.Errorf("the sidecar's promtail tenant_id is not %q — reads would miss the stream if Loki gains auth_enabled",
			defaultAuditTenant)
	}
}
