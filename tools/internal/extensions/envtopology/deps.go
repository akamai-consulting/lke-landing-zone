package envtopology

import (
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
)

// Deps carries what this package cannot reach for itself.
//
// Checked against the three-clause rule: can the package already do this with a
// grant it holds? is it a pure function? is it already injectable elsewhere?
// `validateEnvName` failed the second (pure, localised below) and the
// instance-layout reads failed the first — internal/instancelayout already owns
// them and this package imports it directly.
type Deps struct {
	// Exec captures a command's stdout — `gh`, for the branch-policy writes and the
	// environment listing.
	Exec func(name string, args ...string) ([]byte, error)

	// ExecArgv runs a command with stdin piped, for the `gh api` calls that PUT a
	// branch policy. Separate from Exec because it writes rather than reads, which
	// is the distinction the declaration turns on.
	ExecArgv func(argv []string, stdin string) error

	// Summary appends to a GitHub Actions file. The HA-role resolver publishes
	// role= and peer= as step outputs, which is the entire contract the workflow
	// downstream of it reads.
	Summary func(envVar string, lines ...string) error

	// InstanceRepo returns `instance_repo` from .copier-answers.yml, or "" when
	// absent. Narrowed to the one field, as internal/promote's is: handing over the
	// whole answers struct would put the copier-answers model on this side of the
	// boundary to answer a one-line question.
	InstanceRepo func() string

	// Render regenerates the instance's tfvars and apl-values after a spec edit.
	// `llz env set` writes the declarative source and then re-renders, so this is
	// the second half of every mutation here.
	Render func(env string) error

	// DryRun suppresses every mutation and prints what would have run — the one
	// field of package main's globalOpts this package uses.
	DryRun bool

	// LoadSpec reads the instance's LandingZone spec.
	LoadSpec func() (*clusterspec.LandingZone, bool, error)

	// PromoteDeps hands internal/promote its capabilities, for the `--ordered`
	// listing that sorts deployments by promotion_rank. Passed through rather than
	// rebuilt: promote owns what it needs, and this package only asks it a question.
	PromoteDeps func() promote.Deps
}

// deps is the installed capability set. Defaults do something harmless and
// non-nil — hand zero values that work.
var caps = Deps{
	Exec:         func(string, ...string) ([]byte, error) { return nil, nil },
	ExecArgv:     func([]string, string) error { return nil },
	Summary:      realGHAAppend,
	InstanceRepo: func() string { return "" },
	Render:       func(string) error { return nil },
	LoadSpec:     func() (*clusterspec.LandingZone, bool, error) { return nil, false, nil },
	PromoteDeps:  func() promote.Deps { return promote.Deps{} },
}

// Install wires the capabilities main owns. Call once, before any verb runs.
func Install(d Deps) { caps = d }

// validateEnvName returns an error if env is not a legal topo.Deployment name. The
// contract (^[a-z][a-z0-9-]{1,30}$) lives in internal/validate so the LandingZone
// spec validator enforces the identical rule; this is a one-line delegation, not a
// seam.
func validateEnvName(env string) error { return validate.EnvName(env) }

// firstNonEmpty returns the first non-empty string. Pure, localised.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// realGHAAppend is the DEFAULT Summary, and it does the real thing.
//
// It started as `func(string, ...string) error { return nil }` and broke
// TestWriteHAResolution immediately: that test asserts the HA resolver publishes
// role= and peer= to GITHUB_OUTPUT, and a no-op default writes nothing, so the
// assertion ran against an empty file.
//
// Third time this exact bug has been caught by a test in this branch (teardown's
// Summary, objenc's SecretField, converge's installed default). The rule is
// settled: AN INSTALLED DEFAULT IS A FIXTURE TOO. Defaulting a capability to
// "return nil" makes every un-installed caller untestable in the same way a no-op
// stub does.
func realGHAAppend(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}
