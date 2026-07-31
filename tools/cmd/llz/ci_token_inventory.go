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
	// tokenStateAbsent — secretEntry only. The API ANSWERED and said 404: this
	// credential is genuinely not configured. Distinct from `unknown`, which here
	// means the API would not answer (403/5xx) and we therefore know nothing.
	//
	// Collapsing the two is the exact defect this whole ADR-0012 series was
	// written about, one level down. `llz_credential_configured = 0` is read by
	// LLZCredentialUnconfigured as "seed this credential"; publishing it for a
	// credential we merely could not READ turns a token-permission problem into a
	// page that names the wrong thing and sends the operator to the wrong runbook.
	// The risk is not theoretical: the five OpenBao credentials added here are
	// infra-<region> ENVIRONMENT secrets, whose metadata needs different token
	// permissions from the repo-scoped ones, and the probe had never once read an
	// environment-scoped secret in production — it never ran at all.
	tokenStateAbsent = "absent"
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
	Class     string `json:"class"`  // rotation class for the age gauge
	Expect    string `json:"expect"` // present | optional | absent — see credExpect*
	State     string `json:"state"`
}

// Whether a credential is SUPPOSED to be configured. Age is only half of what
// this probe can see: the other half is presence — and presence is NOT uniformly
// good, which is why this is three values and not a bool. One credential's
// healthy state is absent, and two are healthy either way.
const (
	// credExpectPresent — the instance cannot function without it, so a 404 is a
	// finding. Everything except the root token.
	credExpectPresent = "present"
	// credExpectOptional — legitimately absent on some healthy deployments, so
	// neither presence nor absence is a finding. Measured when present (the age is
	// real and worth seeing); silent when not.
	//
	// This is the Harbor robot pair. `llz ci seed-harbor-standby` returns early
	// with "HARBOR_ROBOT_NAME / HARBOR_PASSWORD not yet published — the active
	// peer's harbor-robot-provisioner CronJob sets them once Harbor is up", so a
	// STANDBY peer, and any deployment before Harbor first comes up, does not have
	// them — by design, not by omission. Classing them `present` (as the first
	// draft of this did) would fire LLZCredentialUnconfigured and FAIL the daily
	// credential job on a perfectly healthy standby.
	credExpectOptional = "optional"
	// credExpectAbsent — OPENBAO_ROOT_TOKEN. Bootstrap mints a root token, uses
	// it, and REVOKES it (ci_bao_breakglass.go: "a root token is ephemeral by
	// design"); what survives is the 3-of-5 recovery quorum. So a root token
	// sitting in an Actions secret in steady state is a live full-admin
	// credential nobody revoked after a break-glass — the presence IS the
	// finding, and the remedy is `bao-breakglass --action revoke`.
	credExpectAbsent = "absent"
)

// tokenInventory is the ConfigMap payload the reconciler reads (data["inventory.json"]).
type tokenInventory struct {
	Updated int64         `json:"updated"` // unix time the inventory was written (heartbeat)
	Region  string        `json:"region,omitempty"`
	Tokens  []tokenEntry  `json:"tokens"`
	Secrets []secretEntry `json:"secrets,omitempty"`
	// SecretProbe reports whether the GitHub secrets-metadata probe could run at
	// all: `ok` | `unavailable`. Empty from an inventory written before this
	// field existed, which the reconciler treats as "cannot tell" rather than as
	// either verdict.
	//
	// It exists because an empty Secrets list is AMBIGUOUS and the ambiguity was
	// load-bearing. `Secrets: []` means either "every credential was measured and
	// none exist" or "the probe never ran", and the second is what actually
	// happened for the whole life of ADR 0009's mechanism: the one job that runs
	// `llz ci token-inventory` set OPENBAO_SECRETS_WRITE_TOKEN but never GH_REPO,
	// so newSecretAgeWriter returned an error, the command printed a ::warning::
	// nobody reads, and not one write-time series was ever published. Failing soft
	// was the right call (an instance without a GitHub token still gets its Linode
	// and PAT entries); failing soft INVISIBLY was not.
	SecretProbe string `json:"secret_probe,omitempty"`
}

// Verdicts for tokenInventory.SecretProbe.
const (
	secretProbeOK          = "ok"
	secretProbeUnavailable = "unavailable"
)

// ghSecretTargets are the credentials measured by WRITE TIME rather than expiry.
// They have no expiry (key pairs, passphrases, raw key material) and they cannot
// be tracked the usual way — via an OpenBao `updated_time` — because each one is
// circular with respect to the OpenBao that would hold it: the state-backend
// credentials guard the state that reaches the cluster OpenBao runs in, and the
// seal key, recovery quorum and root token ARE OpenBao's own escrow. Storing any
// of them in OpenBao means losing all of them together. See docs/adr/0009.
//
// The write-time probe is the mechanism ADR 0009 built for the first three. The
// rest were left behind not because a different mechanism was needed but because
// the target list was a literal nobody revisited — and that is the whole reason
// `llz ci credential-coverage-guard` now exists.
var ghSecretTargets = []struct {
	name   string
	class  string
	expect string
}{
	// ── the state backend (ADR 0009) ──────────────────────────────────────────
	// Operator-dispatchable via secret-rotation.yml scope=tf-state-key.
	{"TF_STATE_ACCESS_KEY", credClassOnDemand, credExpectPresent},
	{"TF_STATE_SECRET_KEY", credClassOnDemand, credExpectPresent},
	// Was `static` — correctly, while re-encrypting every state file had no
	// automation. scope=state-passphrase is that automation, so the age is now
	// ACTIONABLE and belongs on the 90d SLA rather than the yearly nudge.
	{"TF_STATE_ENCRYPTION_PASSPHRASE", credClassOnDemand, credExpectPresent},

	// ── OpenBao's own escrow ─────────────────────────────────────────────────
	//
	// These are the highest-consequence credentials the platform holds and NONE
	// of them was on the single pane. Their absence is not an oversight of degree:
	// the state-backend trio was measured because ADR 0009 went looking for
	// credentials with no expiry, and these have no expiry EITHER — they were
	// simply not in the list it wrote.
	//
	// ASSUMPTION, checked rather than assumed: `expect: present` on these four
	// encodes "OpenBao is deployed". `openbao` is NOT a Mandatory component
	// (clusterspec/components.go marks only argocd and clusterFoundation), so an
	// instance CAN set components.openbao.enabled=false — and there these four are
	// never seeded, so they would page and fail the daily gate.
	//
	// Left as `present` rather than made conditional, because that shape is already
	// unsupported by the job in question: the daily credential run does
	// `alert-eval --strict`, LLZCredentialRotationOverdue names
	// llz_credential_age_days, and the openbao-gauges lane publishes nothing
	// without OpenBao — so --strict already classes it DEAD? and fails, today,
	// before any of this. Building conditional expectation plumbing for a
	// deployment nothing else supports would be machinery for a shape that cannot
	// pass the surrounding checks anyway. Recorded so the next reader knows it was
	// weighed, not missed.
	//
	// OPENBAO_SEAL_KEY is the AES-256 key the chart's `seal "static"` auto-unseal
	// uses (ci_bao_seed_seal_key.go). It is the encryption-at-rest key for
	// everything in OpenBao's raft store, so it is the single credential whose
	// compromise reads every other credential in the platform. `static`: rotating
	// it means a seal rewrap of the whole store, which nothing here implements —
	// so the yearly nudge is the honest signal, not a 90d SLA nobody can meet.
	{"OPENBAO_SEAL_KEY", credClassStatic, credExpectPresent},
	// The 3-of-5 recovery quorum that authorizes `operator generate-root`. Losing
	// these means break-glass is impossible — which is exactly why an ABSENT one
	// has to be visible (see llz_credential_configured): the failure surfaces on
	// the day you need it and not before.
	{"OPENBAO_RECOVERY_KEY_1", credClassStatic, credExpectPresent},
	{"OPENBAO_RECOVERY_KEY_2", credClassStatic, credExpectPresent},
	{"OPENBAO_RECOVERY_KEY_3", credClassStatic, credExpectPresent},
	// Expected ABSENT — see credExpectAbsent. `on-demand` because there IS a
	// rotation path (`bao-breakglass --action rotate`); the age matters only in
	// the state this credential is not supposed to be in.
	{"OPENBAO_ROOT_TOKEN", credClassOnDemand, credExpectAbsent},

	// ── Harbor robots: the standby channel ───────────────────────────────────
	//
	// secret/harbor/robot and secret/harbor/pull-robot are already age-tracked in
	// OpenBao (credPaths, `static`). These are the SECOND copy — published to
	// GitHub by the provisioner so a rebuilt or standby cluster can adopt the
	// existing robots instead of minting new ones (ci_harbor.go's EXISTING_*
	// path). A second copy is a second thing that ages, and nothing was watching
	// it: an OpenBao-side re-seed that failed to republish here leaves the standby
	// channel holding a dead credential, and the OpenBao age would look fine.
	// OPTIONAL, not present — see credExpectOptional. On a standby peer these are
	// published by the ACTIVE peer's provisioner and are absent until it has run.
	{"HARBOR_PASSWORD", credClassStatic, credExpectOptional},
	{"HARBOR_PULL_PASSWORD", credClassStatic, credExpectOptional},
}

// The class is the SAME vocabulary the OpenBao age sampler uses
// (reconcile_openbao.go), deliberately: these series are published as
// llz_credential_age_days too, so LLZCredentialRotationOverdue picks them up with
// no rule change. That is also why the class must track reality rather than
// ambition — `on-demand` on a credential with no dispatchable rotation would page
// an operator who has nothing to dispatch. The three state-backend entries have
// one (secret-rotation.yml scopes `tf-state-key` and `state-passphrase`) and so
// does the root token (`bao-breakglass --action rotate`); the seal key, the
// recovery quorum and the Harbor robot copies do NOT, which is precisely why
// they are `static` and draw the yearly nudge instead.

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
	// The verdict is recorded whether or not the probe could run — see SecretProbe.
	inv.Secrets = gatherSecretAges(d.secretEnv, d.secretProbe)
	inv.SecretProbe = secretProbeVerdict(d.secretProbe != nil, inv.Secrets)
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
		// Default `absent`, not `unknown`: the loop below only reaches its end
		// having ASKED. An error downgrades it — never the other way round.
		e := secretEntry{Name: t.name, Class: t.class, Expect: t.expect, State: tokenStateAbsent}
		unreadable := false
		for _, scope := range []string{env, ""} {
			if scope == "" && env == "" {
				continue // already tried the repo scope
			}
			ts, ok, err := probe(scope, t.name)
			if err != nil {
				// A 404 is NOT an error here — SecretUpdatedAt returns (‥, false,
				// nil) for it. So reaching this branch means the API refused to
				// answer: a 403 on the environment scope, a 5xx, a transport
				// failure. We learn nothing about the credential, and saying
				// "absent" would be a claim we cannot support.
				fmt.Fprintf(os.Stderr, "::warning::token-inventory: %s (%s): %v\n", t.name, scopeLabel(scope), err)
				unreadable = true
				continue
			}
			if ok {
				e.Scope, e.UpdatedAt, e.State = scopeLabel(scope), ts, tokenStateOK
				break
			}
		}
		// Found in one scope, refused in the other, is still found: only downgrade
		// when nothing answered affirmatively anywhere.
		if e.State != tokenStateOK && unreadable {
			e.State = tokenStateUnknown
		}
		if e.Scope == "" {
			e.Scope = scopeLabel(env)
		}
		out = append(out, e)
	}
	return out
}

// secretProbeVerdict decides whether the write-time lane can be trusted this
// run. `ok` requires BOTH that the client was built and that every credential
// got an answer.
//
// The second half is the one that is easy to miss. A client that authenticates
// for repo-scoped secrets can still be refused on the environment scope — they
// are different permissions — and a per-credential 403 leaves that credential
// unmeasured while everything else looks healthy. Reporting `ok` there would
// vouch for a lane that is partly dark, which is the failure this field exists
// to make impossible.
func secretProbeVerdict(clientBuilt bool, secrets []secretEntry) string {
	if !clientBuilt {
		return secretProbeUnavailable
	}
	for _, s := range secrets {
		if s.State == tokenStateUnknown {
			return secretProbeUnavailable
		}
	}
	return secretProbeOK
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
