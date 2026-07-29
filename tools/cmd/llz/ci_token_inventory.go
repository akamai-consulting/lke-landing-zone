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
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
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

// secretEntry is one GitHub Actions secret's WRITE time — the age signal for
// credentials that have no expiry to read and cannot live in OpenBao (the
// state-backend key pair, the state-encryption passphrase). Metadata only:
// GitHub Actions secrets are write-only over the API, so there is no value to
// carry even if we wanted to. UpdatedAt is RFC3339; empty means "not configured".
type secretEntry struct {
	Name      string `json:"name"`
	Scope     string `json:"scope"` // repo | infra-<deployment>
	UpdatedAt string `json:"updated_at,omitempty"`
	Class     string `json:"class"` // rotation class for the age gauge
	State     string `json:"state"`
}

// tokenInventory is the ConfigMap payload the reconciler reads (data["inventory.json"]).
type tokenInventory struct {
	Updated int64         `json:"updated"` // unix time the inventory was written (heartbeat)
	Region  string        `json:"region,omitempty"`
	Tokens  []tokenEntry  `json:"tokens"`
	Secrets []secretEntry `json:"secrets,omitempty"`
}

// ghSecretTargets are the credentials measured by WRITE TIME rather than expiry.
// They have no expiry (an Object Storage key pair and a passphrase), and they
// cannot be tracked the usual way — via an OpenBao `updated_time` — because
// OpenBao runs inside the cluster whose state these very credentials guard. See
// docs/adr/0009.
var ghSecretTargets = []struct {
	name  string
	class string
}{
	// Operator-dispatchable via secret-rotation.yml scope=tf-state-key.
	{"TF_STATE_ACCESS_KEY", credClassOnDemand},
	{"TF_STATE_SECRET_KEY", credClassOnDemand},
	// No rotation path today: rotating the passphrase means re-encrypting every
	// state file. `static` is therefore honest — the age is worth SEEING, but
	// nobody can act on it yet, so it must not page as overdue. When a rollover
	// path exists this becomes on-demand, which is a one-word change here.
	{"TF_STATE_ENCRYPTION_PASSPHRASE", credClassStatic},
}

// The class is the SAME vocabulary the OpenBao age sampler uses
// (reconcile_openbao.go), deliberately: these series are published as
// llz_credential_age_days too, so LLZCredentialRotationOverdue picks them up with
// no rule change. That is also why the class must track reality rather than
// ambition — `on-demand` on a credential with no dispatchable rotation would page
// an operator who has nothing to dispatch.

// ghPATTargets declares the GitHub service PATs the inventory measures. It was
// two hardcoded literals at the call site, which is why `E2E_DISPATCH_TOKEN` and
// `GHCR_READ_TOKEN` — both real PATs with a readable expiry — were never on the
// credential single pane at all.
//
// `optional` is the load-bearing field. gatherGitHubTokens classifies an unset
// token as state=unknown rather than dropping it, which is exactly right for a
// PAT every instance must have (its absence is a finding) and exactly wrong for
// one most instances deliberately don't set: a stock instance would grow two
// permanent `unknown` rows on the dashboard, and an inventory that always shows
// unknowns is one nobody reads. Optional targets are therefore skipped when
// unset, and measured identically when present.
var ghPATTargets = []struct {
	name     string // the env var, and the `token` label on the metric
	optional bool
}{
	// Always expected: the two service PATs bootstrap and apl-core run on.
	{name: "OPENBAO_SECRETS_WRITE_TOKEN"},
	{name: "APL_VALUES_REPO_TOKEN"},
	// Template-repo admin only — the e2e harness dispatch PAT. Absent on every
	// adopter instance.
	{name: "E2E_DISPATCH_TOKEN", optional: true},
	// Only a PRIVATE fork/image needs a GHCR read credential; the first-party
	// charts are public, so a stock instance leaves it empty by design.
	{name: "GHCR_READ_TOKEN", optional: true},
}

// ghTargetsFromEnv resolves ghPATTargets against the process environment,
// dropping optional targets whose secret is not set.
func ghTargetsFromEnv(api string) []patTarget {
	out := make([]patTarget, 0, len(ghPATTargets))
	for _, t := range ghPATTargets {
		v := os.Getenv(t.name)
		if v == "" && t.optional {
			continue
		}
		out = append(out, patTarget{name: t.name, api: api, token: v})
	}
	return out
}

func ciTokenInventoryCmd() *cobra.Command {
	var namespace, name string
	var maxDays, warnDays int
	c := &cobra.Command{
		Use:   "token-inventory",
		Short: "measure CI-token expiry and emit the ConfigMap the reconciler re-exposes as metrics",
		Long: "Writer half of the credential single-pane-of-glass. Measures the expiry of the\n" +
			"external CI tokens this job holds — the GitHub service PATs in ghPATTargets\n" +
			"(OPENBAO_SECRETS_WRITE_TOKEN, APL_VALUES_REPO_TOKEN, and E2E_DISPATCH_TOKEN /\n" +
			"GHCR_READ_TOKEN when set) via the token-expiration header, and every Linode PAT\n" +
			"via the Linode API — and emits a ConfigMap (metadata only, never a token value) to\n" +
			"stdout. Pipe it to `kubectl apply -f -`; the in-cluster llz-reconciler re-exposes it\n" +
			"as llz_token_expiry_timestamp_seconds so Prometheus alerts before expiry.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The canonical PAT reader, but an absent token is NOT fatal here:
			// buildTokenInventory skips the Linode section on "" and still
			// reports the GitHub PATs.
			linodeToken, _ := ciToken()
			// Secret-age probe: metadata only, and only when a GitHub token +
			// repo are available. GitHub Actions secrets are write-only over the
			// API, so this cannot read a value even in principle.
			if w, err := newSecretAgeWriter(); err != nil {
				fmt.Fprintf(os.Stderr, "::warning::token-inventory: secret-age probe unavailable (%v) — token entries still written.\n", err)
			} else {
				secretAgeProbe = w.SecretUpdatedAt
			}
			inv := buildTokenInventory(cmd.Context(), tokenInvDeps{
				ghTargets:   ghTargetsFromEnv(envOr("GITHUB_API", "https://api.github.com")),
				linodeToken: linodeToken,
				secretEnv:   secretScopeForRegion(os.Getenv("REGION")),
				secretProbe: secretAgeProbe,
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
	secretEnv   string
	secretProbe func(env, name string) (string, bool, error)
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
	// Write-time ages for the credentials with no expiry to read (ADR 0009).
	inv.Secrets = gatherSecretAges(d.secretEnv, d.secretProbe)
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

// newSecretAgeWriter builds the metadata client from the same GH_TOKEN/GH_REPO
// the rest of the CI family uses. Best-effort by design: an instance without a
// token still gets its Linode + GitHub PAT entries.
func newSecretAgeWriter() (*forge.GitHubSecretWriter, error) {
	tok := firstNonEmpty(os.Getenv("GH_TOKEN"), os.Getenv("OPENBAO_SECRETS_WRITE_TOKEN"))
	repo := os.Getenv("GH_REPO")
	if tok == "" || repo == "" {
		return nil, fmt.Errorf("GH_TOKEN/GH_REPO not set")
	}
	return forge.NewGitHubSecretWriter(envOr("GITHUB_API", "https://api.github.com"), tok, repo)
}

// secretAgeProbe reads one Actions secret's write time. Injected so the gather
// logic is unit-testable without GitHub.
var secretAgeProbe func(env, name string) (string, bool, error)

// gatherSecretAges measures the WRITE time of each ghSecretTargets entry. Env
// secrets are looked up first (these are infra-<deployment> scoped), then the
// repo scope, because an instance may hold either.
//
// An absent secret is reported with state=unknown and no timestamp rather than
// dropped. That is the property the OpenBao path could not give us: the sampler
// there skips a 404 as "not seeded yet", so a never-written credential is
// indistinguishable from a healthy one. Here the API distinguishes them, so a
// missing state-backend credential is visible instead of silent.
func gatherSecretAges(env string, probe func(env, name string) (string, bool, error)) []secretEntry {
	if probe == nil {
		return nil
	}
	out := make([]secretEntry, 0, len(ghSecretTargets))
	for _, t := range ghSecretTargets {
		e := secretEntry{Name: t.name, Class: t.class, State: tokenStateUnknown}
		for _, scope := range []string{env, ""} {
			if scope == "" && env == "" {
				continue // already tried the repo scope
			}
			ts, ok, err := probe(scope, t.name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "::warning::token-inventory: %s (%s): %v\n", t.name, scopeLabel(scope), err)
				continue
			}
			if ok {
				e.Scope, e.UpdatedAt, e.State = scopeLabel(scope), ts, tokenStateOK
				break
			}
		}
		if e.Scope == "" {
			e.Scope = scopeLabel(env)
		}
		out = append(out, e)
	}
	return out
}

// secretScopeForRegion maps a deployment to the GitHub environment its
// credentials live in. Empty region → repo scope only.
func secretScopeForRegion(region string) string {
	if strings.TrimSpace(region) == "" {
		return ""
	}
	return "infra-" + region
}

func scopeLabel(env string) string {
	if env == "" {
		return "repo"
	}
	return env
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
