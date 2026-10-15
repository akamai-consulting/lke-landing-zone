package assertobs

// extension.go — `assert-observability` declares itself.
//
// TWENTY-FIFTH EXTENSION, AND THE LARGEST: twelve files, ~2,700 lines, eleven
// cobra verbs. The catalog's biggest `assert-*` row, and the measurement had a
// surprise in it — adding `ci_readiness.go` to the file set LOWERED the closure
// from 20 to 14. The Loki readiness helpers looked like a separate concern and
// were the other half of this one, so pulling them in removed more edges than it
// added. That is the opposite of how every other file-list correction has gone.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `assert-observability` declaration.
//
//	assertion:verified   "telemetry"          [cluster-read]
//	assertion:verified   "prom-rules"         [read-repo]
//
// IT ONCE DECLARED A THIRD BINDING, `transition:converged "harbor-ca-retrofit"`
// [cluster-read, cluster-write], for a Harbor CA retrofit buried in ci_readiness.go
// that ran `kubectl rollout restart` so registry pods would trust the obj-proxy CA.
// It went with the SSE-C gateway; the verb that triggered it is a no-op now, so
// the extension no longer writes to the cluster and no longer says it does.
//
// WHY prom-rules IS SEPARATE. It lints PrometheusRule YAML with promtool against
// files in the repo — no cluster, so `read-repo`. Folding it into the telemetry
// binding would hand a file-linter a cluster-read grant it never uses.
//
// THE REST GENUINELY OBSERVE. Scrape targets, alert evaluation, alert delivery,
// log ingestion, Grafana dashboards and the Loki write proof all read. The Loki
// flush probe POSTs to an ingester's /flush endpoint, which mutates Loki's
// internal state but nothing this model has a grant for — it is not the cluster,
// not the cloud, and not a credential. Recorded here rather than forced into the
// nearest word.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "assert-observability",
		Short:  "the telemetry stack works: targets scraped, alerts evaluated and delivered, logs ingested, dashboards present",
		Always: true,
		// FOLLOWS `observability`. Every lane this extension owns reads the telemetry
		// stack itself — Loki bootstrapped, ServiceMonitors scraped, alerts evaluated
		// and delivered, dashboards labelled. With the component off there is no
		// Prometheus to query and no Loki to ship to, so these assertions are not
		// failing, they are asking about something the instance does not have.
		Component: "observability",
		Bindings: []extension.Binding{
			{
				Kind:   extension.Assertion,
				Name:   "telemetry",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ClusterRead},
			},
			{
				Kind:   extension.Assertion,
				Name:   "prom-rules",
				State:  extension.Verified,
				Grants: []extension.Grant{extension.ReadRepo},
			},
		},
	}
}

// promRulesBinding is the assertion the rules walk reads through. Looked up by
// NAME rather than by kind: this extension declares two assertions and only one
// is read-repo, so looking it up by kind could hand back the cluster-read one.
func promRulesBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Assertion && b.Name == "prom-rules" {
			return b
		}
	}
	panic("assert-observability: no assertion:prom-rules binding — the rules walk " +
		"builds its read-repo reader from it, so its absence is a wiring bug")
}
