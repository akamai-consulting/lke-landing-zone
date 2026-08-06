package assertobs

// loki_query.go — shared read access to the in-cluster Loki HTTP API, for the
// `assert-openbao-audit` gate. Same transport as prom_query.go (one ephemeral
// `kubectl port-forward` per command; the apiserver Service proxy is
// webhook-denied on LKE-Enterprise), with two Loki-specific pieces: the tenant
// header and the query_range envelope.
//
// THE TENANT HEADER IS ALWAYS SENT, AND IT IS LOAD-BEARING — not, as this comment
// used to claim, inert.
//
// The old text said apl-core ships Loki with `auth_enabled` unset (single-tenant),
// so the header was ignored and any value would do. That is wrong. Read off
// lke638084, monitoring/loki has `auth_enabled: true`, and a read with no header
// answers "no org id" rather than serving anything.
//
// So the tenant PARTITIONS reads, and each caller must send the one its own writer
// used. There is more than one writer: the OpenBao promtail sidecar pushes
// `tenant_id: platform`, while apl-core's collector routes by namespace and puts
// everything outside a team namespace under `admins`. A single shared default is
// therefore wrong for one of them by construction — see defaultAuditTenant and
// defaultCollectorTenant, which is why they are two constants.
//
// (docs/playbooks/loki-access.md used to contradict itself here — it said both "do
// not add the header" and "note the mandatory header", and told operators that a
// 401 "no org id" was NOT a tenancy problem. It now carries the routing table
// above; keep the two in step.)

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harborauth"
)

// WithLoki opens a single kubectl port-forward to the Loki gateway named by spec
// ("<namespace>/<service>:<port>"), invokes fn with a getter bound to it, and
// tears the forward down on return. Package var so tests can seam it.
var WithLoki = func(spec, tenant string, fn func(get func(apiPath string) ([]byte, error)) error) error {
	return withForwardedAPI(spec, forwardedService{
		name:      "Loki",
		readyPath: "/ready",
		headers:   map[string]string{"X-Scope-OrgID": tenant},
	}, fn)
}

// LokiEntry is one log line and the timestamp Loki stored it under.
// EXPORTED because the openbao-audit assert lane in package main parses the same
// Loki response shape. One decoder, so a change to how Loki reports entries
// cannot be applied in one lane and missed in the other.
type LokiEntry struct {
	At   time.Time
	Line string
}

// LokiStream is one label-set and its entries from a query_range response.
type LokiStream struct {
	Labels  map[string]string
	Entries []LokiEntry
}

// LokiQueryRangePath builds the query_range URL for a LogQL selector over
// [start, end]. Timestamps go as nanosecond epochs — Loki accepts RFC3339 too,
// but the ns integer has no timezone/precision ambiguity to get wrong. Pure.
func LokiQueryRangePath(selector string, start, end time.Time, limit int) string {
	v := url.Values{}
	v.Set("query", selector)
	v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	v.Set("limit", strconv.Itoa(limit))
	// Newest first: with a limit, backward keeps the entries we report the FRESH
	// ones, which is the whole question the gate asks.
	v.Set("direction", "backward")
	return "/loki/api/v1/query_range?" + v.Encode()
}

// ParseLokiStreams decodes a query_range response into streams.
//
// A parse failure is an ERROR, not an empty result. That distinction is the
// whole point: "the pipeline delivered nothing" and "we could not tell" are
// different answers, and collapsing them into `nil` is how a gate passes
// vacuously on a response it never understood. Likewise a non-`streams`
// resultType (a metric query reached here, or Loki changed shape) is an error
// rather than zero streams.
func ParseLokiStreams(raw []byte) ([]LokiStream, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decoding Loki response: %w (%s)", err, harborauth.TruncateForError(raw))
	}
	if resp.Status != "" && resp.Status != "success" {
		return nil, fmt.Errorf("query_range: Loki returned status %q: %s", resp.Status, harborauth.TruncateForError(raw))
	}
	if rt := resp.Data.ResultType; rt != "" && rt != "streams" {
		return nil, fmt.Errorf("query_range: Loki returned resultType %q, expected \"streams\" — the selector is not a log query", rt)
	}
	out := make([]LokiStream, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		s := LokiStream{Labels: r.Stream}
		for _, v := range r.Values {
			if len(v) < 2 {
				continue
			}
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("decoding Loki entry timestamp %q: %w", v[0], err)
			}
			s.Entries = append(s.Entries, LokiEntry{At: time.Unix(0, ns), Line: v[1]})
		}
		out = append(out, s)
	}
	return out, nil
}
