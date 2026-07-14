package main

// Shared model for "what does an e2e-ready instance need, and what's already
// there?" — used by both `llz doctor` (report) and `llz tokens` (skip what's
// satisfied). GitHub exposes variable VALUES but only secret NAMES, so we can
// prepopulate vars.env with real values and, for secrets, only know presence.

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// requirement is one var/secret an e2e instance needs.
type requirement struct {
	Name     string
	Secret   bool   // secret (value not readable) vs variable (value readable)
	EnvScope bool   // infra-<env> environment vs repo-level
	Required bool   // required for a green e2e vs optional
	Template bool   // lives on the template repo (admin/e2e-harness) vs the instance repo
	How      string // one-line: how the wizard provides it
}

// e2eRequirements is the single source of truth. admin adds the template-repo
// e2e-harness entries.
func e2eRequirements(admin bool) []requirement {
	reqs := []requirement{
		{"LINODE_API_TOKEN", true, true, true, false, "Linode PAT (also creates the state bucket)"},
		{"TF_STATE_ACCESS_KEY", true, true, true, false, "bucket-scoped OBJ key (created)"},
		{"TF_STATE_SECRET_KEY", true, true, true, false, "bucket-scoped OBJ key (created)"},
		{"OPENBAO_SECRETS_WRITE_TOKEN", true, true, true, false, "GitHub PAT, Actions+Secrets:write"},
		{"APL_VALUES_REPO_TOKEN", true, true, true, false, "GitHub fine-grained PAT, Contents:write (values+apps repo)"},
		{"TF_STATE_BUCKET", false, false, true, false, "state bucket name (created)"},
		{"TF_STATE_ENDPOINT", false, false, true, false, "S3 endpoint of the chosen cluster"},
		{"TF_IMAGE", false, false, true, false, "ghcr.io/<org>/ci-terraform:<tag> (computed)"},
		{"KUBE_IMAGE", false, false, true, false, "ghcr.io/<org>/ci-kubernetes:<tag> (computed)"},
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
			requirement{"E2E_INSTANCE_REPO", false, false, true, true, "the example repo"},
			requirement{"E2E_LINODE_REGION", false, false, true, true, "region of the chosen cluster"},
			requirement{"E2E_OBJ_CLUSTER", false, false, true, true, "the chosen OBJ cluster"},
			requirement{"E2E_DISPATCH_TOKEN", true, false, true, true, "classic PAT scopes repo+workflow (Contents+Actions:write + workflow files) on the example repo"},
		)
	}
	return reqs
}

// liveState is the configured-on-GitHub state of one repo. Variable values are
// captured; secrets are presence-only. Env maps cover the infra-<env> scope.
type liveState struct {
	repoVars    map[string]string
	repoSecrets map[string]bool
	envVars     map[string]string
	envSecrets  map[string]bool
}

// has reports whether name is configured at all (env scope falls back to
// repo-level, mirroring GitHub's resolution for environment jobs).
func (s liveState) has(name string, secret bool) bool {
	if secret {
		return s.envSecrets[name] || s.repoSecrets[name]
	}
	_, okEnv := s.envVars[name]
	_, okRepo := s.repoVars[name]
	return okEnv || okRepo
}

// value returns a variable's configured value (env scope wins), "" if unset.
func (s liveState) value(name string) string {
	if v, ok := s.envVars[name]; ok {
		return v
	}
	return s.repoVars[name]
}

// fetchLiveState queries repo + infra-<env> via gh. Missing env / 404s yield
// empty maps rather than errors (a fresh repo has no environment yet).
func fetchLiveState(repo, env string) liveState {
	s := liveState{
		repoVars: map[string]string{}, repoSecrets: map[string]bool{},
		envVars: map[string]string{}, envSecrets: map[string]bool{},
	}
	for _, v := range ghVars("repos/" + repo + "/actions/variables") {
		s.repoVars[v.Name] = v.Value
	}
	for _, n := range ghSecretNames("repos/" + repo + "/actions/secrets") {
		s.repoSecrets[n] = true
	}
	if env != "" {
		for _, v := range ghVars("repos/" + repo + "/environments/infra-" + env + "/variables") {
			s.envVars[v.Name] = v.Value
		}
		for _, n := range ghSecretNames("repos/" + repo + "/environments/infra-" + env + "/secrets") {
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

func ghSecretNames(path string) []string {
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
	out, err := execOutput("gh", "api", path)
	if err != nil {
		return nil
	}
	return out
}

// satisfied reports whether req is met by either the local .llz/*.env or the
// live repo state — the same predicate doctor reports and the wizard skips on.
func satisfied(req requirement, secrets, vars map[string]string, st liveState) bool {
	if req.Secret {
		if _, ok := secrets[req.Name]; ok {
			return true
		}
	} else {
		if _, ok := vars[req.Name]; ok {
			return true
		}
	}
	return st.has(req.Name, req.Secret)
}

// prepopulateVars seeds vars.env with variable VALUES already on the repo
// (instance + template) that aren't set locally — so the wizard reuses them
// instead of recomputing/reprompting. Returns how many it filled in.
func prepopulateVars(vars map[string]string, reqs []requirement, instance, template liveState) int {
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
		if v := st.value(r.Name); v != "" {
			vars[r.Name] = v
			n++
		}
	}
	return n
}

// reportReadiness prints the e2e-readiness table (doctor + the wizard's plan)
// and returns the names of REQUIRED items still missing.
// reportReadiness prints the plan and returns the REQUIRED items that are not yet
// configured ON GITHUB. Status reflects GitHub reality, not the local .llz cache:
// a value present only in the cache shows "cached → will push" and still counts
// as not-done, so the wizard pushes it instead of declaring "nothing to do".
// (satisfied()/have() stay cache-aware so we don't re-prompt for cached values.)
// The `validity` map (name → probe verdict, from probeTokenValidities) drives the
// VALID column; pass nil to omit active probing (the column then reads "unprobed"
// for every credential).
func reportReadiness(reqs []requirement, secrets, vars map[string]string, instance, template liveState, validity map[string]tokenValidity) []string {
	var missing []string
	fmt.Printf("\n%s\n", bold(fmt.Sprintf("%-30s %-7s %-9s %-24s %s", "NAME", "KIND", "REQUIRED", "STATUS", "VALID")))
	for _, r := range reqs {
		st := instance
		if r.Template {
			st = template
		}
		onGitHub := st.has(r.Name, r.Secret)
		_, inCache := vars[r.Name]
		if r.Secret {
			_, inCache = secrets[r.Name]
		}
		statusPlain, statusColor := "✗ missing", red
		switch {
		case onGitHub:
			statusPlain, statusColor = "✓ set", green
		case inCache:
			statusPlain, statusColor = "⤴ cached → will push", yellow
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
		if !ok || (tv.status != vInvalid && tv.status != vWarn && tv.status != vUnreachable) {
			continue
		}
		fmt.Printf("  %s %s: %s\n", validGlyph(tv.status), r.Name, tv.detail)
	}
	return missing
}

// validCell renders a requirement's VALID column: a short colored verdict. Long
// detail goes in the per-problem notes printed after the table. Every credential
// (kind != none) gets a verdict — never a bare "n/a".
func validCell(r requirement, onGitHub bool, validity map[string]tokenValidity) (string, func(string) string) {
	if kindFor(r.Name) == kindNone {
		return "", dim // not a credential — blank column
	}
	tv, ok := validity[r.Name]
	if !ok {
		return "· unprobed", dim
	}
	switch tv.status {
	case vValid:
		return "✓ valid", green
	case vWarn:
		return "⚠ warn", yellow
	case vInvalid:
		return "✗ INVALID", red
	case vUnreachable:
		return "⚠ unreachable", yellow
	default: // vSkipped — not probed because the value isn't available locally
		if onGitHub {
			return "· not cached", dim // set on GitHub, value not in .llz cache
		}
		return "· not set", dim
	}
}

func validGlyph(s validityStatus) string {
	switch s {
	case vInvalid:
		return red("✗")
	case vWarn:
		return yellow("⚠")
	default:
		return yellow("⚠")
	}
}

// padColor right-pads a plain string to a display width (rune count — the status
// glyphs render one cell wide) THEN colors it, so the zero-width ANSI escapes
// don't throw off column alignment (the same trick record() uses).
func padColor(plain string, color func(string) string, width int) string {
	if n := width - utf8.RuneCountInString(plain); n > 0 {
		plain += strings.Repeat(" ", n)
	}
	return color(plain)
}

// loadEnvFiles reads the gathered .llz/*.env (empty maps if absent).
func loadEnvFiles() (secrets, vars map[string]string) {
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
