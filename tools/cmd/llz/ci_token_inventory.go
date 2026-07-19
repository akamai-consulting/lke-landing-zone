package main

// ci_token_inventory.go implements `llz ci token-inventory` — the WRITER half of
// the credential single-pane-of-glass. External CI tokens (GitHub service PATs,
// Linode account PATs) live outside the cluster, so only a job that holds them can
// measure their expiry. This command measures it — reusing the same expiry ladders
// the gh-pat-expiry / cred-audit checks use — and writes a ConfigMap the in-cluster
// llz-reconciler re-exposes as `llz_token_expiry_timestamp_seconds` gauges (see
// reconcile_tokens.go), so Prometheus can alert BEFORE a token expires and Grafana
// shows one pane for tokens + certs.
//
// SECURITY: the ConfigMap carries only METADATA — provider, name, expiry unix time,
// and a coarse state — never a token value. It is emitted to stdout as a ConfigMap
// (JSON, which kubectl apply accepts); the scheduled-checks job pipes it to
// `kubectl apply`. The measurement (network) is separated from the rendering (pure)
// so both are unit-tested via the injected ghPATProbe var + credLister interface.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// tokenState is the coarse verdict the reconciler turns into llz_token_audit_ok:
// breach → 0 (pages: no-expiry / expired / over-policy / invalid), everything else → 1.
const (
	tokenStateOK      = "ok"      // expiry set and within policy
	tokenStateWarn    = "warn"    // within the warn window (drives lead-time alerts)
	tokenStateBreach  = "breach"  // no-expiry / expired / over-policy / invalid — audit failure
	tokenStateUnknown = "unknown" // not set / unreachable / unparseable — can't verify, don't page
)

// tokenEntry is one credential's inventory record. Expiry is unix seconds, 0 when
// unknown or the token never expires (the latter is also state=breach).
type tokenEntry struct {
	Provider string `json:"provider"` // github | linode
	Name     string `json:"name"`
	Expiry   int64  `json:"expiry"`
	State    string `json:"state"`
}

// tokenInventory is the ConfigMap payload the reconciler reads (data["inventory.json"]).
type tokenInventory struct {
	Updated int64        `json:"updated"` // unix time the inventory was written (heartbeat)
	Region  string       `json:"region,omitempty"`
	Tokens  []tokenEntry `json:"tokens"`
}

func ciTokenInventoryCmd() *cobra.Command {
	var namespace, name string
	var maxDays, warnDays int
	c := &cobra.Command{
		Use:   "token-inventory",
		Short: "measure CI-token expiry and emit the ConfigMap the reconciler re-exposes as metrics",
		Long: "Writer half of the credential single-pane-of-glass. Measures the expiry of the\n" +
			"external CI tokens this job holds — GitHub service PATs (OPENBAO_SECRETS_WRITE_TOKEN,\n" +
			"APL_VALUES_REPO_TOKEN) via the token-expiration header, and every Linode account PAT\n" +
			"via the Linode API — and emits a ConfigMap (metadata only, never a token value) to\n" +
			"stdout. Pipe it to `kubectl apply -f -`; the in-cluster llz-reconciler re-exposes it\n" +
			"as llz_token_expiry_timestamp_seconds so Prometheus alerts before expiry.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The canonical PAT reader, but an absent token is NOT fatal here:
			// buildTokenInventory skips the Linode section on "" and still
			// reports the GitHub PATs.
			linodeToken, _ := ciToken()
			inv := buildTokenInventory(cmd.Context(), tokenInvDeps{
				ghTargets: []patTarget{
					{"OPENBAO_SECRETS_WRITE_TOKEN", envOr("GITHUB_API", "https://api.github.com"), os.Getenv("OPENBAO_SECRETS_WRITE_TOKEN")},
					{"APL_VALUES_REPO_TOKEN", envOr("GITHUB_API", "https://api.github.com"), os.Getenv("APL_VALUES_REPO_TOKEN")},
				},
				linodeToken: linodeToken,
				newLinode:   func(t string) credLister { return linode.NewClient(t, 30*time.Second) },
				region:      os.Getenv("REGION"),
				now:         time.Now(),
				maxDays:     maxDays,
				warnDays:    warnDays,
			})
			out, err := renderInventoryConfigMap(inv, namespace, name)
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&namespace, "namespace", "llz-reconciler", "namespace of the inventory ConfigMap the reconciler reads")
	f.StringVar(&name, "name", "llz-token-inventory", "name of the inventory ConfigMap")
	f.IntVar(&maxDays, "max-days", 90, "flag a token whose lifetime exceeds this many days as a breach")
	f.IntVar(&warnDays, "warn-days", 14, "mark a token expiring within this many days as warn")
	return c
}

// tokenInvDeps are the injected inputs so buildTokenInventory is unit-testable
// without GitHub/Linode network access.
type tokenInvDeps struct {
	ghTargets   []patTarget
	linodeToken string
	newLinode   func(token string) credLister
	region      string
	now         time.Time
	maxDays     int
	warnDays    int
}

// buildTokenInventory measures every configured token's expiry and assembles the
// inventory. Best-effort: a provider that errors contributes its measurable entries
// and is otherwise skipped (the inventory-stale alert covers a wholesale funnel break).
func buildTokenInventory(ctx context.Context, d tokenInvDeps) tokenInventory {
	inv := tokenInventory{Updated: d.now.Unix(), Region: d.region}
	inv.Tokens = append(inv.Tokens, gatherGitHubTokens(d.ghTargets, d.now, d.maxDays, d.warnDays)...)
	if d.linodeToken != "" {
		if entries, err := gatherLinodeTokens(ctx, d.newLinode(d.linodeToken), d.now, int64(d.maxDays), int64(d.warnDays)); err == nil {
			inv.Tokens = append(inv.Tokens, entries...)
		} else {
			fmt.Fprintf(os.Stderr, "::warning::token-inventory: Linode token list failed (%v) — GitHub entries still written.\n", err)
		}
	}
	sort.Slice(inv.Tokens, func(i, j int) bool {
		if inv.Tokens[i].Provider != inv.Tokens[j].Provider {
			return inv.Tokens[i].Provider < inv.Tokens[j].Provider
		}
		return inv.Tokens[i].Name < inv.Tokens[j].Name
	})
	return inv
}

// gatherGitHubTokens probes each configured GitHub PAT for its expiry header and
// maps the classification to an inventory entry. A masked token value never leaves.
func gatherGitHubTokens(targets []patTarget, now time.Time, maxDays, warnDays int) []tokenEntry {
	var out []tokenEntry
	for _, tgt := range targets {
		present := tgt.token != ""
		code, expHeader := 0, ""
		if present {
			fmt.Fprintf(os.Stderr, "::add-mask::%s\n", tgt.token)
			if c, h, err := ghPATProbe(tgt.api, tgt.token); err == nil {
				code, expHeader = c, h
			}
		}
		state, _ := health.ClassifyPATResponse(present, code, expHeader, now, maxDays, warnDays)
		var expiry int64
		if t, ok := health.ParseExpiryTime(expHeader); ok {
			expiry = t.Unix()
		}
		out = append(out, tokenEntry{Provider: "github", Name: tgt.name, Expiry: expiry, State: patStateToInventory(state)})
	}
	return out
}

// patStateToInventory collapses a health.PATCheckState into the coarse inventory state.
func patStateToInventory(s health.PATCheckState) string {
	switch s {
	case health.PATOK:
		return tokenStateOK
	case health.PATWarn:
		return tokenStateWarn
	case health.PATInvalid, health.PATNoExpiry, health.PATExpired, health.PATOverPolicy:
		return tokenStateBreach
	default: // PATNotSet, PATUnreachable, PATUnparseable
		return tokenStateUnknown
	}
}

// gatherLinodeTokens lists every account PAT and classifies its expiry the same way
// cred-audit does (no-expiry / expired / over-policy → breach; near-expiry → warn).
func gatherLinodeTokens(ctx context.Context, client credLister, now time.Time, maxDays, warnDays int64) ([]tokenEntry, error) {
	tokens, err := client.ListProfileTokens(ctx)
	if err != nil {
		return nil, err
	}
	nowU := now.Unix()
	var out []tokenEntry
	for _, t := range tokens {
		name := linode.MapString(t, "label")
		if id := tokenID(t["id"]); id != "" {
			name = id + ":" + name
		}
		expiry, hasExpiry := linode.ParseTS(linode.MapString(t, "expiry"))
		created, hasCreated := linode.ParseTS(linode.MapString(t, "created"))
		state := tokenStateOK
		switch {
		case !hasExpiry:
			state = tokenStateBreach // never-expiring PAT
		case expiry <= nowU:
			state = tokenStateBreach // already expired
		case hasCreated && expiry-created > maxDays*linode.DaySecs:
			state = tokenStateBreach // lifetime exceeds policy
		case expiry-nowU <= warnDays*linode.DaySecs:
			state = tokenStateWarn
		}
		var exp int64
		if hasExpiry {
			exp = expiry
		}
		out = append(out, tokenEntry{Provider: "linode", Name: name, Expiry: exp, State: state})
	}
	return out, nil
}

// tokenID stringifies a Linode token id, which arrives as a JSON number (float64)
// or occasionally a string. Empty for anything else — the id just prefixes the
// display name for uniqueness.
func tokenID(v any) string {
	switch id := v.(type) {
	case float64:
		return strconv.FormatInt(int64(id), 10)
	case string:
		return id
	default:
		return ""
	}
}

// renderInventoryConfigMap marshals the inventory into a ConfigMap (as JSON, which
// kubectl apply accepts). data["inventory.json"] carries the payload; SSA-friendly
// labels let apl-core's tooling recognize it. Pure — unit-tested.
func renderInventoryConfigMap(inv tokenInventory, namespace, name string) (string, error) {
	payload, err := json.Marshal(inv)
	if err != nil {
		return "", err
	}
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/part-of":    "platform",
				"app.kubernetes.io/managed-by": "llz-token-inventory",
			},
		},
		"data": map[string]any{"inventory.json": string(payload)},
	}
	b, err := json.MarshalIndent(cm, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
