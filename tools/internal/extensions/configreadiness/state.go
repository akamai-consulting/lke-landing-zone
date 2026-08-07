package configreadiness

// Shared model for "what does an e2e-ready instance need, and what's already
// there?" — used by both `llz doctor` (report) and `llz tokens` (skip what's
// Satisfied). GitHub exposes variable VALUES but only secret NAMES, so we can
// prepopulate vars.env with real values and, for secrets, only know presence.

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// requirement is one var/secret an e2e instance needs.
type Requirement struct {
	Name     string
	Secret   bool   // secret (value not readable) vs variable (value readable)
	EnvScope bool   // infra-<env> environment vs repo-level
	Required bool   // required for a color.Green e2e vs optional
	Template bool   // lives on the template repo (admin/e2e-harness) vs the instance repo
	How      string // one-line: how the wizard provides it
}

// E2ERequirements is the single source of truth. admin adds the template-repo
// e2e-harness entries.
func E2ERequirements(admin bool) []Requirement {
	reqs := []Requirement{
		{"LINODE_API_TOKEN", true, true, true, false, "Linode PAT (also creates the state bucket)"},
		{"TF_STATE_ACCESS_KEY", true, true, true, false, "bucket-scoped OBJ key (created)"},
		{"TF_STATE_SECRET_KEY", true, true, true, false, "bucket-scoped OBJ key (created)"},
		{"OPENBAO_SECRETS_WRITE_TOKEN", true, true, true, false, "GitHub PAT, Actions+Secrets:write"},
		{"APL_VALUES_REPO_TOKEN", true, true, true, false, "GitHub fine-grained PAT, Contents:write (values+apps repo)"},
		// REPO-LEVEL (EnvScope false), unlike every other secret here. One instance
		// has ONE state-encryption passphrase: the key-provider name it writes under
		// is a single repo variable (TF_STATE_ENCRYPTION_KEY_NAME), and a rotation
		// re-keys every root of every deployment together. GitHub resolves a
		// repo-level secret inside an infra-<env> job, so one value covers them all —
		// whereas an env-scoped copy would let a second deployment be provisioned
		// with a DIFFERENT passphrase and quietly diverge from that model.
		{statePassphraseSecret, true, false, true, false, "OpenTofu state encryption — generated + escrowed (ADR 0007)"},
		{"TF_STATE_BUCKET", false, false, true, false, "state bucket name (created)"},
		{"TF_STATE_ENDPOINT", false, false, true, false, "S3 endpoint of the chosen cluster"},
		{"TF_IMAGE", false, false, true, false, "ghcr.io/<org>/ci-tofu:sha-<template pin> (computed)"},
		{"KUBE_IMAGE", false, false, true, false, "ghcr.io/<org>/ci-kubernetes:sha-<template pin> (computed)"},
		{"LINODE_DNS_TOKEN", true, true, false, false, "Linode Domains:RW (cert DNS-01)"},
		{"HARBOR_URL", false, false, false, false, "Harbor base URL"},
		// GHCR read credential — OPTIONAL: the first-party charts are public, so
		// leave EMPTY for a stock instance. Only a PRIVATE fork / private image
		// needs it. Tracked here (not hand-set) so `llz doctor` shows it and, when
		// present, actively validates it — a stale GHCR PAT otherwise 403s the chart
		// pre-flight. Env-scoped, paired: read:packages secret + its owner (var).
		{"GHCR_READ_TOKEN", true, true, false, false, "GitHub read:packages PAT (ONLY for a private fork/image; empty for public charts)"},
		{"GHCR_USERNAME", false, true, false, false, "owner of GHCR_READ_TOKEN (only with it)"},
	}
	if admin {
		reqs = append(reqs,
			Requirement{"E2E_INSTANCE_REPO", false, false, true, true, "the example repo"},
			Requirement{"E2E_LINODE_REGION", false, false, true, true, "region of the chosen cluster"},
			Requirement{"E2E_OBJ_CLUSTER", false, false, true, true, "the chosen OBJ cluster"},
			Requirement{"E2E_DISPATCH_TOKEN", true, false, true, true, "classic PAT scopes repo+workflow (Contents+Actions:write + workflow files) on the example repo"},
		)
	}
	return reqs
}

// SecretIsEnvScoped reports whether a secret belongs in the infra-<env>
// environment rather than at repo level. Unknown names default to env-scoped,
// which is what every instance secret was before the requirement table carried a
// repo-level one.
func SecretIsEnvScoped(name string) bool {
	for _, r := range E2ERequirements(true) {
		if r.Name == name && r.Secret {
			return r.EnvScope
		}
	}
	return true
}

// liveState is the configured-on-GitHub state of one repo. Variable values are
// captured; secrets are presence-only. Env maps cover the infra-<env> scope.
type LiveState struct {
	repoVars    map[string]string
	repoSecrets map[string]bool
	envVars     map[string]string
	envSecrets  map[string]bool
}

// has reports whether name is configured at all (env scope falls back to
// repo-level, mirroring GitHub's resolution for environment jobs).
func (s LiveState) Has(name string, secret bool) bool {
	if secret {
		return s.envSecrets[name] || s.repoSecrets[name]
	}
	_, okEnv := s.envVars[name]
	_, okRepo := s.repoVars[name]
	return okEnv || okRepo
}

// NewLiveState builds a LiveState from its four maps.
//
// The fields stay unexported: a caller that can see them can also ask questions
// the type has no answer for, and the env→repo fallback in Has/Value is the whole
// point of the type. But `llz doctor` legitimately needs to CONSTRUCT one — it
// renders the readiness table from a state it fetched itself — so a constructor is
// the narrow way to allow that without opening the maps.
func NewLiveState(repoVars map[string]string, repoSecrets map[string]bool,
	envVars map[string]string, envSecrets map[string]bool) LiveState {
	return LiveState{repoVars: repoVars, repoSecrets: repoSecrets, envVars: envVars, envSecrets: envSecrets}
}

// HasRepoSecret reports whether name is set as a REPO-level secret specifically.
//
// Distinct from Has, which falls back env→repo. The wizard needs the narrow
// question for E2E_DISPATCH_TOKEN: it is a repo-level secret by design, and an
// env-scoped one of the same name would not serve the dispatch it gates. Added as
// an accessor rather than by exporting repoSecrets — a caller that can see the map
// can also ask questions the type has no answer for.
func (s LiveState) HasRepoSecret(name string) bool { return s.repoSecrets[name] }

// value returns a variable's configured value (env scope wins), "" if unset.
func (s LiveState) Value(name string) string {
	if v, ok := s.envVars[name]; ok {
		return v
	}
	return s.repoVars[name]
}

// FetchLiveState queries repo + infra-<env> via gh. Missing env / 404s yield
// empty maps rather than errors (a fresh repo has no environment yet).
func FetchLiveState(repo, env string) LiveState {
	s := LiveState{
		repoVars: map[string]string{}, repoSecrets: map[string]bool{},
		envVars: map[string]string{}, envSecrets: map[string]bool{},
	}
	for _, v := range ghVars("repos/" + repo + "/actions/variables") {
		s.repoVars[v.Name] = v.Value
	}
	for _, n := range GHSecretNames("repos/" + repo + "/actions/secrets") {
		s.repoSecrets[n] = true
	}
	if env != "" {
		for _, v := range ghVars("repos/" + repo + "/environments/infra-" + env + "/variables") {
			s.envVars[v.Name] = v.Value
		}
		for _, n := range GHSecretNames("repos/" + repo + "/environments/infra-" + env + "/secrets") {
			s.envSecrets[n] = true
		}
	}
	return s
}

type ghVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func ghVars(path string) []ghVar {
	var out struct {
		Variables []ghVar `json:"variables"`
	}
	_ = json.Unmarshal(ghAPI(path), &out)
	return out.Variables
}

func GHSecretNames(path string) []string {
	var out struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	_ = json.Unmarshal(ghAPI(path), &out)
	names := make([]string, 0, len(out.Secrets))
	for _, s := range out.Secrets {
		names = append(names, s.Name)
	}
	return names
}

// ghAPI runs `gh api <path>` and returns stdout (nil on error — callers treat a
// failed/absent endpoint as "nothing configured").
func ghAPI(path string) []byte {
	out, err := deps.Exec("gh", "api", path)
	if err != nil {
		return nil
	}
	return out
}

// Satisfied reports whether req is met by either the local .llz/*.env or the
// live repo state — the same predicate doctor reports and the wizard skips on.
func Satisfied(req Requirement, secrets, vars map[string]string, st LiveState) bool {
	if req.Secret {
		if _, ok := secrets[req.Name]; ok {
			return true
		}
	} else {
		if _, ok := vars[req.Name]; ok {
			return true
		}
	}
	return st.Has(req.Name, req.Secret)
}

// PrepopulateVars seeds vars.env with variable VALUES already on the repo
// (instance + template) that aren't set locally — so the wizard reuses them
// instead of recomputing/reprompting. Returns how many it filled in.
func PrepopulateVars(vars map[string]string, reqs []Requirement, instance, template LiveState) int {
	n := 0
	for _, r := range reqs {
		if r.Secret {
			continue
		}
		if _, ok := vars[r.Name]; ok {
			continue
		}
		st := instance
		if r.Template {
			st = template
		}
		if v := st.Value(r.Name); v != "" {
			vars[r.Name] = v
			n++
		}
	}
	return n
}

// ReportReadiness prints the e2e-readiness table (doctor + the wizard's plan)
// and returns the names of REQUIRED items still missing.
// ReportReadiness prints the plan and returns the REQUIRED items that are not yet
// configured ON GITHUB. Status reflects GitHub reality, not the local .llz cache:
// a value present only in the cache shows "cached → will push" and still counts
// as not-done, so the wizard pushes it instead of declaring "nothing to do".
// (Satisfied()/have() stay cache-aware so we don't re-prompt for cached values.)
// The `validity` map (name → probe verdict, from probeTokenValidities) drives the
// VALID column; pass nil to omit active probing (the column then reads "unprobed"
// for every credential).
func ReportReadiness(reqs []Requirement, secrets, vars map[string]string, instance, template LiveState, validity map[string]tokeninv.TokenValidity) []string {
	var missing []string
	fmt.Printf("\n%s\n", color.Bold(fmt.Sprintf("%-30s %-7s %-9s %-24s %s", "NAME", "KIND", "REQUIRED", "STATUS", "VALID")))
	for _, r := range reqs {
		st := instance
		if r.Template {
			st = template
		}
		onGitHub := st.Has(r.Name, r.Secret)
		_, inCache := vars[r.Name]
		if r.Secret {
			_, inCache = secrets[r.Name]
		}
		statusPlain, statusColor := "✗ missing", color.Red
		switch {
		case onGitHub:
			statusPlain, statusColor = "✓ set", color.Green
		case inCache:
			statusPlain, statusColor = "⤴ cached → will push", color.Yellow
		}
		if r.Template {
			statusPlain += " (template)"
		}
		kind := "var"
		if r.Secret {
			kind = "secret"
		}
		req := "optional"
		if r.Required {
			req = "REQUIRED"
		}
		validPlain, validColor := validCell(r, onGitHub, validity)
		fmt.Printf("%-30s %-7s %-9s %s %s\n", r.Name, kind, req, padColor(statusPlain, statusColor, 24), validColor(validPlain))
		if r.Required && !onGitHub {
			missing = append(missing, r.Name)
		}
	}
	// Detail notes only for the actionable verdicts (INVALID / warnings) — kept out
	// of the columnar table so it stays aligned.
	for _, r := range reqs {
		tv, ok := validity[r.Name]
		if !ok || (tv.Status != tokeninv.VInvalid && tv.Status != tokeninv.VWarn && tv.Status != tokeninv.VUnreachable) {
			continue
		}
		fmt.Printf("  %s %s: %s\n", validGlyph(tv.Status), r.Name, tv.Detail)
	}
	return missing
}

// validCell renders a requirement's VALID column: a short colored verdict. Long
// detail goes in the per-problem notes printed after the table. Every credential
// (kind != none) gets a verdict — never a bare "n/a".
func validCell(r Requirement, onGitHub bool, validity map[string]tokeninv.TokenValidity) (string, func(string) string) {
	if tokeninv.KindFor(r.Name) == tokeninv.KindNone {
		return "", color.Dim // not a credential — blank column
	}
	tv, ok := validity[r.Name]
	if !ok {
		return "· unprobed", color.Dim
	}
	switch tv.Status {
	case tokeninv.VValid:
		return "✓ valid", color.Green
	case tokeninv.VWarn:
		return "⚠ warn", color.Yellow
	case tokeninv.VInvalid:
		return "✗ INVALID", color.Red
	case tokeninv.VUnreachable:
		return "⚠ unreachable", color.Yellow
	default: // vSkipped — not probed because the value isn't available locally
		if onGitHub {
			return "· not cached", color.Dim // set on GitHub, value not in .llz cache
		}
		return "· not set", color.Dim
	}
}

func validGlyph(s tokeninv.ValidityStatus) string {
	switch s {
	case tokeninv.VInvalid:
		return color.Red("✗")
	case tokeninv.VWarn:
		return color.Yellow("⚠")
	default:
		return color.Yellow("⚠")
	}
}

// padColor right-pads a plain string to a display width (rune count — the status
// glyphs render one cell wide) THEN colors it, so the zero-width ANSI escapes
// don't throw off column alignment (the same trick record() uses).
func padColor(plain string, paint func(string) string, width int) string {
	if n := width - utf8.RuneCountInString(plain); n > 0 {
		plain += strings.Repeat(" ", n)
	}
	return paint(plain)
}

// LoadEnvFiles reads the gathered .llz/*.env (empty maps if absent).
func LoadEnvFiles() (secrets, vars map[string]string) {
	secrets = readEnvFile(".llz/secrets.env")
	vars = readEnvFile(".llz/vars.env")
	if secrets == nil {
		secrets = map[string]string{}
	}
	if vars == nil {
		vars = map[string]string{}
	}
	return secrets, vars
}
