package configreadiness

import (
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

// Deps carries what this package cannot reach for itself.
//
// FOUR FIELDS, and every one of them is a genuine capability — checked against
// the three-clause rule the last three extractions paid for: can the package
// already do this with a grant it holds? is it a pure function rather than a
// capability? is it already injectable somewhere else?
//
// Six other symbols were in the measured closure and NONE of them became a Deps
// field. `orAll`, `validateEnvName`, `tfvarsValue` and `firstNonEmpty` are pure
// functions (localised below or in the file that uses them);
// `statePassphraseSecret` is a const; `readEnvFile` is a file read this package
// is already permitted to do. Applying the rule removed six of ten candidates.
// INSTALLED, not threaded. readiness.go's findings are leaf predicates several
// frames below the entry point — the same shape internal/converge documented, and
// the same tradeoff: global mutable state that tests must restore. Threading is
// still the default (see internal/promote, two frames); this is the exception.
type Deps struct {
	// Exec captures a command's stdout — `gh` and `linode-cli`, for the live-state
	// half of readiness. A capability: it reaches outside the repo.
	Exec func(name string, args ...string) ([]byte, error)

	// CloudToken reads the Linode PAT. A function rather than a string because it
	// can fail, and "no token configured" is a readiness FINDING here rather than
	// an error to die on — which is the whole shape of this extension.
	CloudToken func() (string, error)

	// LoadSpec reads the instance's LandingZone spec.
	LoadSpec func() (*clusterspec.LandingZone, bool, error)

	// CheckManifestDrift reports whether the committed apl-values still match what
	// `llz render` would produce. Owned by the render path — this package asks the
	// question and does not know how to answer it.
	CheckManifestDrift func(lz *clusterspec.LandingZone, aplDir string, envs []string) error
}

// orAll renders an empty scope as "(all)" for operator-facing output. A pure
// three-liner localised rather than injected: package main's copy is in reap.go,
// and there is no behaviour to drift.
func orAll(s string) string {
	if s == "" {
		return "(all)"
	}
	return s
}

// firstNonEmpty returns the first non-empty string. Same reasoning as orAll.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── localised pure helpers: copies, not seams ───────────────────────────────

// statePassphraseSecret is the GitHub secret name the Terraform roots read the
// state-encryption passphrase from. A const, not a seam — this package needs to
// know the NAME to report on its presence, not the value.
const statePassphraseSecret = "TF_STATE_ENCRYPTION_PASSPHRASE"

// validateEnvName returns an error if env is not a legal deployment name.
// The deployment-name contract (^[a-z][a-z0-9-]{1,30}$) lives in internal/validate
// so the LandingZone spec validator enforces the IDENTICAL rule. Deployments are
// created dynamically (there is no hardcoded env list — terraform.yml's region
// input is a free-form string, not a choice), so build/status/tokens validate the
// SHAPE of the name, not membership in a fixed set: otherwise `llz env add
// myteam-dev` would succeed but `llz build myteam-dev` refuse.
// Localised pure helpers, all copies rather than seams.
func validateEnvName(env string) error { return validate.EnvName(env) }

// tfvarsValue returns the first `key = "value"` assignment in tfvars content
// (quotes stripped, comments ignored) — the same first-wins grep/sed
// semantics as internal/terraform.ParseTFVars, for keys outside its fixed
// struct (obj_cluster).
func tfvarsValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		i := strings.IndexByte(line, '=')
		if i < 0 || strings.TrimSpace(line[:i]) != key {
			continue
		}
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 && val[0] == '"' {
			if j := strings.IndexByte(val[1:], '"'); j >= 0 {
				return val[1 : 1+j]
			}
		}
		return val
	}
	return ""
}

// readEnvFile parses KEY=value lines, ignoring blanks and # comments. Missing
// file → empty map.
func readEnvFile(path string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = v
		}
	}
	return m
}

// deps is the installed capability set. Defaults do something harmless and
// non-nil — hand zero values that work.
var deps = Deps{
	Exec:               func(string, ...string) ([]byte, error) { return nil, nil },
	CloudToken:         func() (string, error) { return "", nil },
	LoadSpec:           func() (*clusterspec.LandingZone, bool, error) { return nil, false, nil },
	CheckManifestDrift: func(*clusterspec.LandingZone, string, []string) error { return nil },
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { deps = d }
