package main

// ci_token_inventory.go implements `llz ci token-inventory` — the producer half
// of the credential/token "single pane of glass". It gathers a unified inventory
// of every platform credential it can reach and emits it as Prometheus
// text-exposition metrics on stdout:
//
//   - Linode account PATs + Object Storage keys (reusing the `llz ci cred-audit`
//     expiry ladder so the pane agrees with the GitHub-Actions check),
//   - GitHub service PATs (reusing the `llz ci gh-pat-expiry` header self-check),
//   - a static list of the bootstrap-managed in-cluster secrets (seal key,
//     recovery keys, generated admin passwords, Harbor robots) so non-expiring
//     credentials still appear in the inventory.
//
// The scheduled-checks workflow pushes this output to the in-cluster Prometheus
// Pushgateway (PUT per-region group), Prometheus scrapes it, and a Grafana
// dashboard + token-inventory-alerts.yaml turn it into one inventory + expiry
// pane — including the CI-managed tokens Prometheus cannot otherwise see.
//
// Read-only: it lists and classifies, it never mints or rotates.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// tokenEntry is one row of the inventory. status is the small fixed enum the
// dashboard/alerts key on: ok | warn | breach | static | unknown.
type tokenEntry struct {
	name      string
	kind      string
	source    string
	managedBy string
	status    string
	expiry    int64 // unix; only meaningful when hasExpiry
	hasExpiry bool
}

type tokenInventoryOpts struct {
	maxPATDays int64
	warnDays   int64
	output     string
}

func ciTokenInventoryCmd() *cobra.Command {
	var o tokenInventoryOpts
	c := &cobra.Command{
		Use:   "token-inventory",
		Short: "gather a unified credential inventory and emit Prometheus text-format metrics",
		Long: "Producer for the token single-pane-of-glass. Enumerates Linode PATs + OBJ\n" +
			"keys (same expiry ladder as cred-audit), self-checks the GitHub service PATs\n" +
			"(same header probe as gh-pat-expiry), and lists the static bootstrap-managed\n" +
			"in-cluster secrets, emitting llz_token_* metrics on stdout for the scheduled-\n" +
			"checks workflow to PUT to the in-cluster Pushgateway. Read-only. Linode\n" +
			"enumeration self-skips when LINODE_API_TOKEN is unset; GitHub tokens come from\n" +
			"the environment (OPENBAO_SECRETS_WRITE_TOKEN, APL_VALUES_REPO_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCITokenInventory(o) },
	}
	f := c.Flags()
	f.Int64Var(&o.maxPATDays, "max-pat-days", cli.EnvInt("MAX_PAT_DAYS", 90), "PAT lifetime over this many days is a breach")
	f.Int64Var(&o.warnDays, "warn-days", cli.EnvInt("WARN_DAYS", 14), "warn when a credential expires within this many days")
	f.StringVar(&o.output, "output", "-", "write metrics here ('-' or empty = stdout)")
	return c
}

func runCITokenInventory(o tokenInventoryOpts) error {
	now := time.Now()

	entries := staticTokenInventory()
	entries = append(entries, gatherGitHubTokens(now, int(o.maxPATDays), int(o.warnDays))...)

	if token := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN")); token != "" {
		le, err := gatherLinodeTokens(context.Background(), token, now.Unix(), o.maxPATDays, o.warnDays)
		if err != nil {
			// A Linode-side failure must not blank the whole pane — emit what we
			// have and warn, so the other sources still land.
			fmt.Fprintf(os.Stderr, "::warning::token-inventory: Linode enumeration failed (%v) — emitting the remaining sources\n", err)
		} else {
			entries = append(entries, le...)
		}
	} else {
		fmt.Fprintln(os.Stderr, "::warning::token-inventory: LINODE_API_TOKEN not set — skipping Linode token enumeration")
	}

	var buf bytes.Buffer
	renderTokenMetrics(&buf, entries, now.Unix())

	if o.output == "" || o.output == "-" {
		_, err := os.Stdout.Write(buf.Bytes())
		return err
	}
	return os.WriteFile(o.output, buf.Bytes(), 0o644)
}

// gatherGitHubTokens self-checks each known service PAT via the same probe +
// classification ladder as `llz ci gh-pat-expiry`, mapping the verdict onto the
// inventory status enum and carrying the parsed expiry when present.
func gatherGitHubTokens(now time.Time, maxDays, warnDays int) []tokenEntry {
	apiBase := envOr("GITHUB_API", "https://api.github.com")
	targets := []patTarget{
		{"OPENBAO_SECRETS_WRITE_TOKEN", apiBase, os.Getenv("OPENBAO_SECRETS_WRITE_TOKEN")},
		{"APL_VALUES_REPO_TOKEN", apiBase, os.Getenv("APL_VALUES_REPO_TOKEN")},
	}
	out := make([]tokenEntry, 0, len(targets))
	for _, tgt := range targets {
		present := tgt.token != ""
		code, expHeader := 0, ""
		if present {
			maskGHA(tgt.token)
			if c, h, err := ghPATProbe(tgt.api, tgt.token); err == nil {
				code, expHeader = c, h
			}
		}
		state, _ := health.ClassifyPATResponse(present, code, expHeader, now, maxDays, warnDays)
		e := tokenEntry{name: tgt.name, kind: "github-pat", source: "github", managedBy: "manual", status: ghTokenStatus(state)}
		if exp, ok := health.ParseExpiryTime(expHeader); ok {
			e.expiry, e.hasExpiry = exp.Unix(), true
		}
		out = append(out, e)
	}
	return out
}

// ghTokenStatus collapses the gh-pat-expiry state ladder onto the inventory enum.
func ghTokenStatus(s health.PATCheckState) string {
	switch s.Category() {
	case health.CatFail:
		return "breach"
	case health.CatWarn:
		if s == health.PATWarn {
			return "warn"
		}
		return "unknown" // not-set / unreachable / unparseable
	default:
		return "ok"
	}
}

// gatherLinodeTokens enumerates account PATs (with the cred-audit expiry ladder)
// and Object Storage keys (no API-side age — inventory only; their 120-day SLA is
// enforced by loki-objkey-rotation-health + the Terraform time_rotating trigger).
func gatherLinodeTokens(ctx context.Context, token string, now, maxPATDays, warnDays int64) ([]tokenEntry, error) {
	client := newCredAuditClient(token)
	maxLife := maxPATDays * linode.DaySecs
	warnWindow := warnDays * linode.DaySecs

	tokens, err := client.ListProfileTokens(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tokenEntry, 0, len(tokens))
	for _, t := range tokens {
		label := unlabelled(linode.MapString(t, "label"))
		created, hasCreated := linode.ParseTS(linode.MapString(t, "created"))
		expiry, hasExpiry := linode.ParseTS(linode.MapString(t, "expiry"))
		e := tokenEntry{name: label, kind: "linode-pat", source: "linode", managedBy: linodeManagedBy(label)}
		switch {
		case !hasExpiry:
			e.status = "breach" // no-expiry violates the 90-day policy
		case expiry <= now:
			e.status, e.expiry, e.hasExpiry = "breach", expiry, true
		case hasCreated && expiry-created > maxLife:
			e.status, e.expiry, e.hasExpiry = "breach", expiry, true
		case expiry-now <= warnWindow:
			e.status, e.expiry, e.hasExpiry = "warn", expiry, true
		default:
			e.status, e.expiry, e.hasExpiry = "ok", expiry, true
		}
		out = append(out, e)
	}

	keys, err := client.ListObjectStorageKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		label := unlabelled(linode.MapString(k, "label"))
		out = append(out, tokenEntry{
			name: label, kind: "linode-obj-key", source: "linode",
			managedBy: linodeManagedBy(label), status: "static",
		})
	}
	return out, nil
}

// linodeManagedBy guesses ownership from the resource label: the cred rotator
// mints with the platform's `llz-`/`loki-`/`harbor` prefixes, anything else is a
// human-created token. Heuristic only — drives the managed_by pane column.
func linodeManagedBy(label string) string {
	l := strings.ToLower(label)
	if strings.HasPrefix(l, "llz-") || strings.HasPrefix(l, "loki-") || strings.Contains(l, "harbor") {
		return "cred-rotator"
	}
	return "manual"
}

// staticTokenInventory is the hand-maintained list of in-cluster credentials that
// have no queryable expiry but must still appear in the inventory pane. These are
// static-by-design (seal/recovery keys would brick the cluster if rotated;
// generated admin passwords use ESO IfNotExists) or rotated out-of-band (Harbor
// robots are manual today). Keep in sync with docs/alerting.md.
func staticTokenInventory() []tokenEntry {
	return []tokenEntry{
		{name: "openbao-seal-key", kind: "seal-key", source: "openbao", managedBy: "bootstrap", status: "static"},
		{name: "openbao-recovery-keys", kind: "recovery-key", source: "openbao", managedBy: "bootstrap", status: "static"},
		{name: "openbao-root-token", kind: "root-token", source: "openbao", managedBy: "bootstrap", status: "static"},
		{name: "grafana-admin", kind: "generated-secret", source: "eso", managedBy: "bootstrap", status: "static"},
		{name: "otel-ingress-token", kind: "generated-secret", source: "eso", managedBy: "bootstrap", status: "static"},
		{name: "harbor-robot-ci-firewall", kind: "harbor-robot", source: "harbor", managedBy: "manual", status: "static"},
		{name: "harbor-robot-pull", kind: "harbor-robot", source: "harbor", managedBy: "manual", status: "static"},
	}
}

// renderTokenMetrics writes the inventory as Prometheus text-exposition format.
// region is intentionally NOT a label here — the Pushgateway group key supplies
// it, so each region's pane is its own group and a removed token disappears on
// the next PUT.
func renderTokenMetrics(w io.Writer, entries []tokenEntry, now int64) {
	fmt.Fprintln(w, "# HELP llz_token_inventory_info Inventory entry for a tracked platform credential (always 1).")
	fmt.Fprintln(w, "# TYPE llz_token_inventory_info gauge")
	for _, e := range entries {
		fmt.Fprintf(w, "llz_token_inventory_info{name=\"%s\",kind=\"%s\",source=\"%s\",managed_by=\"%s\"} 1\n",
			escLabel(e.name), escLabel(e.kind), escLabel(e.source), escLabel(e.managedBy))
	}

	fmt.Fprintln(w, "# HELP llz_token_audit_status Current audit status (status: ok|warn|breach|static|unknown).")
	fmt.Fprintln(w, "# TYPE llz_token_audit_status gauge")
	for _, e := range entries {
		fmt.Fprintf(w, "llz_token_audit_status{name=\"%s\",kind=\"%s\",source=\"%s\",managed_by=\"%s\",status=\"%s\"} 1\n",
			escLabel(e.name), escLabel(e.kind), escLabel(e.source), escLabel(e.managedBy), escLabel(e.status))
	}

	fmt.Fprintln(w, "# HELP llz_token_expiry_timestamp_seconds Unix expiry of a credential (emitted only when known).")
	fmt.Fprintln(w, "# TYPE llz_token_expiry_timestamp_seconds gauge")
	for _, e := range entries {
		if !e.hasExpiry {
			continue
		}
		fmt.Fprintf(w, "llz_token_expiry_timestamp_seconds{name=\"%s\",kind=\"%s\",source=\"%s\",managed_by=\"%s\"} %d\n",
			escLabel(e.name), escLabel(e.kind), escLabel(e.source), escLabel(e.managedBy), e.expiry)
	}

	fmt.Fprintln(w, "# HELP llz_token_inventory_push_timestamp_seconds Unix time the inventory was last gathered (freshness heartbeat).")
	fmt.Fprintln(w, "# TYPE llz_token_inventory_push_timestamp_seconds gauge")
	fmt.Fprintf(w, "llz_token_inventory_push_timestamp_seconds %d\n", now)
}

// escLabel escapes a Prometheus label value per the text-exposition spec
// (backslash, double-quote, newline). Our values are simple ASCII names, but a
// Linode token label is user-controlled, so escape defensively.
func escLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
