package tokeninv

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
// so both are unit-tested via the injected tokenprobe.GHPATProbe var + credLister interface.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credtargets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

// ghTargetsFromEnv resolves ghPATTargets against the process environment,
// dropping optional targets whose secret is not set.
func ghTargetsFromEnv(api string) []PATTarget {
	out := make([]PATTarget, 0, len(credtargets.GHPATTargets))
	for _, t := range credtargets.GHPATTargets {
		v := os.Getenv(t.Name)
		if v == "" && t.Optional {
			continue
		}
		out = append(out, PATTarget{name: t.Name, api: api, token: v})
	}
	return out
}

// RunInventory measures token expiry and returns the ConfigMap the reconciler
// re-exposes as metrics. Metadata only — never a token value.
func RunInventory(ctx context.Context, d Deps, namespace, name string, maxDays, warnDays int) (string, error) {
	// The canonical PAT reader, but an absent token is NOT fatal here:
	// buildTokenInventory skips the Linode section on "" and still reports the
	// GitHub PATs.
	linodeToken, _ := d.CloudToken()
	// Secret-age probe: metadata only, and only when a GitHub token + repo are
	// available. GitHub Actions secrets are write-only over the API, so this
	// cannot read a value even in principle.
	if w, err := newSecretAgeWriter(); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::token-inventory: secret-age probe unavailable (%v) — token entries still written.\n", err)
	} else {
		secretAgeProbe = w.SecretUpdatedAt
	}
	inv := buildTokenInventory(ctx, tokenInvDeps{
		ghTargets:   ghTargetsFromEnv(envOr("GITHUB_API", "https://api.github.com")),
		linodeToken: linodeToken,
		secretEnv:   secretScopeForRegion(os.Getenv("REGION")),
		secretProbe: secretAgeProbe,
		newLinode:   func(t string) CredLister { return linode.NewClient(t, 30*time.Second) },
		region:      os.Getenv("REGION"),
		now:         time.Now(),
		maxDays:     maxDays,
		warnDays:    warnDays,
	})
	return renderInventoryConfigMap(inv, namespace, name)
}

// tokenInvDeps are the injected inputs so buildTokenInventory is unit-testable
// without GitHub/Linode network access.
type tokenInvDeps struct {
	ghTargets   []PATTarget
	linodeToken string
	newLinode   func(token string) CredLister
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
func buildTokenInventory(ctx context.Context, d tokenInvDeps) credtargets.Inventory {
	inv := credtargets.Inventory{Updated: d.now.Unix(), Region: d.region}
	inv.Tokens = append(inv.Tokens, gatherGitHubTokens(d.ghTargets, d.now, d.maxDays, d.warnDays)...)
	// Write-time ages for the credentials with no expiry to read (ADR 0009).
	// The verdict is recorded whether or not the probe could run — see SecretProbe.
	inv.Secrets = gatherSecretAges(d.secretEnv, d.secretProbe)
	inv.SecretProbe = credtargets.SecretProbeVerdict(d.secretProbe != nil, inv.Secrets)
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
func gatherGitHubTokens(targets []PATTarget, now time.Time, maxDays, warnDays int) []credtargets.TokenEntry {
	var out []credtargets.TokenEntry
	for _, tgt := range targets {
		present := tgt.token != ""
		code, expHeader := 0, ""
		if present {
			fmt.Fprintf(os.Stderr, "::add-mask::%s\n", tgt.token)
			if c, h, err := tokenprobe.GHPATProbe(tgt.api, tgt.token); err == nil {
				code, expHeader = c, h
			}
		}
		state, _ := health.ClassifyPATResponse(present, code, expHeader, now, maxDays, warnDays)
		var expiry int64
		if t, ok := health.ParseExpiryTime(expHeader); ok {
			expiry = t.Unix()
		}
		out = append(out, credtargets.TokenEntry{Provider: "github", Name: tgt.name, Expiry: expiry, State: patStateToInventory(state)})
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
func gatherSecretAges(env string, probe func(env, name string) (string, bool, error)) []credtargets.SecretEntry {
	if probe == nil {
		return nil
	}
	out := make([]credtargets.SecretEntry, 0, len(credtargets.GHSecretTargets))
	for _, t := range credtargets.GHSecretTargets {
		// Default `absent`, not `unknown`: the loop below only reaches its end
		// having ASKED. An error downgrades it — never the other way round.
		e := credtargets.SecretEntry{Name: t.Name, Class: t.Class, Expect: t.Expect, State: credtargets.TokenStateAbsent}
		unreadable := false
		for _, scope := range []string{env, ""} {
			if scope == "" && env == "" {
				continue // already tried the repo scope
			}
			ts, ok, err := probe(scope, t.Name)
			if err != nil {
				// A 404 is NOT an error here — SecretUpdatedAt returns (‥, false,
				// nil) for it. So reaching this branch means the API refused to
				// answer: a 403 on the environment scope, a 5xx, a transport
				// failure. We learn nothing about the credential, and saying
				// "absent" would be a claim we cannot support.
				fmt.Fprintf(os.Stderr, "::warning::token-inventory: %s (%s): %v\n", t.Name, scopeLabel(scope), err)
				unreadable = true
				continue
			}
			if ok {
				e.Scope, e.UpdatedAt, e.State = scopeLabel(scope), ts, credtargets.TokenStateOK
				break
			}
		}
		// Found in one scope, refused in the other, is still found: only downgrade
		// when nothing answered affirmatively anywhere.
		if e.State != credtargets.TokenStateOK && unreadable {
			e.State = credtargets.TokenStateUnknown
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
		return credtargets.TokenStateOK
	case health.PATWarn:
		return credtargets.TokenStateWarn
	case health.PATInvalid, health.PATNoExpiry, health.PATExpired, health.PATOverPolicy:
		return credtargets.TokenStateBreach
	default: // PATNotSet, PATUnreachable, PATUnparseable
		return credtargets.TokenStateUnknown
	}
}

// gatherLinodeTokens lists every account PAT and classifies its expiry the same way
// cred-audit does (no-expiry / expired / over-policy → breach; near-expiry → warn).
func gatherLinodeTokens(ctx context.Context, client CredLister, now time.Time, maxDays, warnDays int64) ([]credtargets.TokenEntry, error) {
	tokens, err := client.ListProfileTokens(ctx)
	if err != nil {
		return nil, err
	}
	nowU := now.Unix()
	var out []credtargets.TokenEntry
	for _, t := range tokens {
		name := linode.MapString(t, "label")
		if id := tokenID(t["id"]); id != "" {
			name = id + ":" + name
		}
		expiry, hasExpiry := linode.ParseTS(linode.MapString(t, "expiry"))
		created, hasCreated := linode.ParseTS(linode.MapString(t, "created"))
		state := credtargets.TokenStateOK
		switch {
		case !hasExpiry:
			state = credtargets.TokenStateBreach // never-expiring PAT
		case expiry <= nowU:
			state = credtargets.TokenStateBreach // already expired
		case hasCreated && expiry-created > maxDays*linode.DaySecs:
			state = credtargets.TokenStateBreach // lifetime exceeds policy
		case expiry-nowU <= warnDays*linode.DaySecs:
			state = credtargets.TokenStateWarn
		}
		var exp int64
		if hasExpiry {
			exp = expiry
		}
		out = append(out, credtargets.TokenEntry{Provider: "linode", Name: name, Expiry: exp, State: state})
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
func renderInventoryConfigMap(inv credtargets.Inventory, namespace, name string) (string, error) {
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
