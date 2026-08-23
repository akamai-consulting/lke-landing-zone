package assertobs

// ci_assert_log_ingestion.go implements `llz ci assert-log-ingestion` — the gate
// that every landing-zone namespace's pod logs are actually reaching Loki.
//
// assert-openbao-audit proved ONE producer's round trip: the promtail sidecar in
// the OpenBao pod. That sidecar is a first-party thing this repo configures, and
// it broke because we pointed it at a Service that did not exist. The rest of the
// landing zone's logs travel a DIFFERENT path — apl-core's cluster-wide collector
// picks up pod stdout by Kubernetes service discovery — and nothing asserted that
// path at all.
//
// It fails the same silent way. A namespace dropped from the collector's
// discovery config, a relabel rule that stops emitting the `namespace` label, a
// NetworkPolicy that blocks the collector from a namespace: in every case the
// pods stay Running, Loki stays Ready, assert-loki stays color.Green, and the logs you
// need during an incident are simply not there. You find out when you go looking,
// which is the worst possible moment.
//
// The assertion is per-namespace and freshness-bounded: for each expected
// namespace there must be at least one log line in the lookback window. Existence
// alone would stay color.Green for Loki's whole retention after collection stopped —
// the same trap assert-openbao-audit avoids.
//
// FAIL-CLOSED: no stream, no entries, an unparseable response, or an unreachable
// Loki all fail. A log-delivery gate that passes having examined nothing is
// indistinguishable from the outage it exists to find.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// defaultLogNamespaces are the landing-zone namespaces whose pod logs must be
// reaching Loki. Deliberately a KNOWN list rather than "every namespace Loki
// happens to have": if collection regresses at the source, a discovered list
// would come back short and the gate would pass color.Green on the very bug it exists
// to catch — the same reasoning as assert-scrape-targets' monitor list.
//
// Only the two namespaces that exist on EVERY landing zone are defaults.
// Component-gated namespaces (llz-pat-rotator, llz-cluster-health,
// llz-argo-workflows) would fail on a cluster that legitimately does not run
// them; add them via --namespaces where they are enabled.
var defaultLogNamespaces = []string{"llz-reconciler", "llz-openbao"}

// defaultCollectorTenant is the Loki tenant apl-core's cluster-wide collector
// writes THIS path's logs under, and it is deliberately NOT defaultAuditTenant.
//
// Two different producers reach Loki, and they use different tenants. The OpenBao
// sidecar pushes with promtail `tenant_id: platform`, which is what
// assert-openbao-audit reads. apl-core's platform-logs-collector routes by
// namespace instead (its `routing` connector, read off a live cluster):
//
//	namespace == "team-admin"     → X-Scope-OrgID: admin
//	namespace == "team-platform"  → X-Scope-OrgID: platform
//	everything else (default)     → X-Scope-OrgID: admins
//
// Every namespace this gate checks is a landing-zone namespace, none of which is
// a team namespace, so all of them land in the DEFAULT pipeline — tenant
// `admins`. Reading as `platform` found nothing and reported the collector dead
// while it was demonstrably alive: the gateway's access log showed a steady
// stream of `POST /otlp/v1/logs → 204` throughout.
//
// This matters only because apl-core ships Loki with `auth_enabled: true`, so the
// tenant actually partitions reads. A read with no header answers "no org id".
const defaultCollectorTenant = "admins"

// nsIngestion is one namespace's ingestion verdict.
type nsIngestion struct {
	Namespace string
	Entries   int
	Newest    time.Time
	FailWhy   string
}

// logIngestionSelector builds the LogQL selector for a namespace's pod logs.
func logIngestionSelector(ns string) string {
	return fmt.Sprintf(`{namespace=%q}`, ns)
}

// evalNamespaceIngestion reduces one namespace's streams to a verdict. Pure.
//
// Zero entries is a FAILURE, never a pass-with-note. The whole reason this gate
// exists is that "no logs" is what a broken collector and a healthy-but-quiet
// namespace look like from the outside — and on a converged cluster neither
// llz-reconciler (a 30s sample loop that logs every pass) nor llz-openbao is ever
// genuinely silent for the lookback window.
func evalNamespaceIngestion(ns string, streams []LokiStream) nsIngestion {
	v := nsIngestion{Namespace: ns}
	for _, s := range streams {
		for _, e := range s.Entries {
			v.Entries++
			if e.At.After(v.Newest) {
				v.Newest = e.At
			}
		}
	}
	if v.Entries == 0 {
		v.FailWhy = "no log lines in the window — this namespace's pods are not being collected. " +
			"Check the collector's namespace discovery, its relabel rules (a dropped `namespace` label " +
			"makes the stream unfindable rather than absent), and any NetworkPolicy between the collector and this namespace"
	}
	return v
}

// probeLogIngestion queries every expected namespace over one open port-forward.
func probeLogIngestion(get func(string) ([]byte, error), namespaces []string,
	limit int, lookback time.Duration, now time.Time) ([]nsIngestion, error) {

	out := make([]nsIngestion, 0, len(namespaces))
	for _, ns := range namespaces {
		raw, err := get(LokiQueryRangePath(logIngestionSelector(ns), now.Add(-lookback), now, limit))
		if err != nil {
			return nil, err
		}
		streams, perr := ParseLokiStreams(raw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, evalNamespaceIngestion(ns, streams))
	}
	return out, nil
}

// printTenantDiagnosis dumps what the queried tenant can actually see, because
// "no log lines" has two very different causes and the message alone cannot tell
// them apart: nothing is being collected, or we are reading the WRONG TENANT.
//
// That is not hypothetical — it is how this gate first failed. Loki here runs
// auth_enabled: true, the collector files landing-zone namespaces under `admins`,
// and this gate inherited the OpenBao sidecar's `platform`. Every query came back
// empty while the gateway logged a steady stream of successful pushes, and
// finding that out took two rounds of hand-querying a live cluster. An empty
// label set for the tenant we asked is the single most informative thing to print
// here: no labels AT ALL means the tenant is wrong (or Loki is genuinely empty),
// whereas labels present but no lines for a namespace means collection really did
// stop for it.
//
// Best-effort and non-fatal — the verdict is already decided by the time this runs.
func printTenantDiagnosis(loki, tenant string) {
	fmt.Printf("\n-- diagnosis: what does tenant %q actually hold? --\n", tenant)
	err := WithLoki(loki, tenant, func(get func(string) ([]byte, error)) error {
		raw, err := get("/loki/api/v1/labels")
		if err != nil {
			return err
		}
		var out struct {
			Data []string `json:"data"`
		}
		if jerr := json.Unmarshal(raw, &out); jerr != nil {
			fmt.Printf("   (could not parse the label list: %v)\n", jerr)
			return nil
		}
		if len(out.Data) == 0 {
			fmt.Printf("   NO LABELS AT ALL for tenant %q. Either this tenant is wrong or Loki holds nothing.\n"+
				"   Loki runs auth_enabled: true here, so the tenant partitions reads. Check which X-Scope-OrgID the\n"+
				"   PRODUCER writes under: apl-core's collector routes by namespace (see its `routing` connector in\n"+
				"   namespace otel) and files everything outside a team namespace under the DEFAULT pipeline's tenant.\n"+
				"   Override with --tenant once you know it.\n", tenant)
			return nil
		}
		sort.Strings(out.Data)
		fmt.Printf("   labels present (%d): %s\n", len(out.Data), strings.Join(out.Data, ", "))
		fmt.Printf("   The tenant is right and Loki has data, so collection stopped for the namespaces above —\n" +
			"   check the collector's discovery and any NetworkPolicy between it and them.\n")
		return nil
	})
	if err != nil {
		fmt.Printf("   (diagnosis query failed: %v)\n", err)
	}
	fmt.Println()
}

// failedIngestion returns the namespaces that are not being collected.
func failedIngestion(vs []nsIngestion) []string {
	var out []string
	for _, v := range vs {
		if v.FailWhy != "" {
			out = append(out, v.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

func runCIAssertLogIngestion(loki, tenant string, namespaces []string, limit int,
	lookback, settle, interval time.Duration) error {

	fmt.Println("## Landing-zone log-ingestion assertion")
	if len(namespaces) == 0 {
		fmt.Fprintln(os.Stderr, "::error::no --namespaces given — refusing to pass having checked nothing")
		return fmt.Errorf("no --namespaces given — refusing to pass vacuously")
	}
	if lookback <= 0 {
		fmt.Fprintln(os.Stderr, "::error::--lookback must be positive — a zero window can never contain a log line")
		return fmt.Errorf("--lookback must be positive")
	}
	fmt.Printf("Loki %s, tenant %s, namespaces %s, lookback %s\n", loki, tenant, strings.Join(namespaces, ", "), lookback)

	var last []nsIngestion
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		var vs []nsIngestion
		err := WithLoki(loki, tenant, func(get func(string) ([]byte, error)) error {
			var perr error
			vs, perr = probeLogIngestion(get, namespaces, limit, lookback, time.Now())
			return perr
		})
		last, lastErr = vs, err
		if err == nil && len(failedIngestion(vs)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if err != nil {
			fmt.Printf("attempt %d: could not query Loki at %s (%v) — retrying in %s\n", attempt, loki, err, interval)
		} else {
			fmt.Printf("attempt %d: %v still not ingesting — retrying in %s\n", attempt, failedIngestion(vs), interval)
		}
		time.Sleep(interval)
	}

	// An unreachable Loki is a gate FAILURE, not a skip: we cannot tell a Loki we
	// could not reach from a collector that has stopped, and conflating those is
	// exactly what this gate refuses.
	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::could not query Loki at %s within %s (%v)\n", loki, settle, lastErr)
		return fmt.Errorf("could not query Loki at %s within %s: %w", loki, settle, lastErr)
	}

	for _, v := range last {
		if v.FailWhy != "" {
			fmt.Printf("FAIL: %s — %s\n", v.Namespace, v.FailWhy)
		} else {
			fmt.Printf("OK: %s — %d line(s), freshest %s old\n",
				v.Namespace, v.Entries, time.Since(v.Newest).Truncate(time.Second))
		}
	}

	if bad := failedIngestion(last); len(bad) > 0 {
		printTenantDiagnosis(loki, tenant)
		fmt.Fprintf(os.Stderr, "::error::no logs reaching Loki from: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("no logs reaching Loki from (tenant %s): %s", tenant, strings.Join(bad, ", "))
	}
	fmt.Printf("All %d landing-zone namespace(s) are shipping logs to Loki.\n", len(last))
	return nil
}
