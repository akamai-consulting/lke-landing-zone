package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
)

// Deps carries what this package cannot reach for itself.
//
// TWO FIELDS out of four candidates, and the other two were rejected by the
// three-clause rule (can the package already do this with a grant it holds? is it
// a pure function? is it already injectable elsewhere?):
//
//   - `report` is a six-line printer over internal/color — pure, and localised
//     below rather than injected. A seam here would only let a test assert that
//     printing happened, which is the vacuous-fixture trap this campaign has hit
//     four times.
//   - `firstNonEmpty` is three lines of pure string selection, localised for the
//     same reason internal/configreadiness and internal/envtopology localised
//     their copies.
type Deps struct {
	// LoadSpec reads the instance's LandingZone spec — the declared Kubernetes
	// versions this package compares against what Linode actually offers.
	LoadSpec func() (*clusterspec.LandingZone, bool, error)

	// InstanceRepo returns `instance_repo` from .copier-answers.yml, or "" when
	// absent.
	//
	// A STRING, NOT THE `answers` STRUCT, following internal/promote: this package
	// reads exactly one field to learn the repo's OWNER, and taking the whole type
	// would pull package main's copier-answers model across the boundary to answer
	// a one-line question.
	InstanceRepo func() string
}

// caps is the installed capability set, and BOTH DEFAULTS DO THE REAL THING.
//
// The first cut defaulted them to `return nil, false, nil` and `return ""`, and
// two tests failed immediately: one writes a LandingZone spec to a temp dir and
// asserts every env's Kubernetes pin comes back, the other writes a
// .copier-answers.yml and a cross-org workflow and asserts the gate fails. Both
// were asserting against a default that had been handed nothing to read.
//
// FIFTH TIME THIS EXACT BUG HAS BEEN CAUGHT BY A TEST IN THIS BRANCH (teardown's
// Summary, objenc's SecretField, converge's installed default, env-topology's
// realGHAAppend). The rule is settled: AN INSTALLED DEFAULT IS A FIXTURE TOO.
// Defaulting a capability to a zero value makes every un-installed caller
// untestable in the same way a no-op stub does.
var caps = Deps{
	LoadSpec:     realLoadSpec,
	InstanceRepo: realInstanceRepo,
}

// realLoadSpec is the DEFAULT LoadSpec, and it reads the instance's spec.
func realLoadSpec() (*clusterspec.LandingZone, bool, error) {
	tfDir, _, _ := instancelayout.Detect()
	specRoot := filepath.Dir(tfDir)
	if !clusterspec.InstancePresent(specRoot) {
		return nil, false, nil
	}
	lz, err := clusterspec.LoadInstance(specRoot)
	return lz, true, err
}

// realInstanceRepo is the DEFAULT InstanceRepo: `instance_repo` from
// .copier-answers.yml in the working directory, or "" when absent.
func realInstanceRepo() string {
	b, err := os.ReadFile(".copier-answers.yml")
	if err != nil {
		return ""
	}
	var a struct {
		InstanceRepo string `yaml:"instance_repo"`
	}
	if yaml.Unmarshal(b, &a) != nil {
		return ""
	}
	return a.InstanceRepo
}

// Install wires the capabilities main owns. Call once, before any probe runs.
func Install(d Deps) { caps = d }

// report prints one ✓/✗ line. Pure, localised — package main has the same six
// lines for the wizard, and there is no behaviour to drift.
func report(name string, ok bool) {
	mark := color.Red("✗")
	if ok {
		mark = color.Green("✓")
	}
	fmt.Printf("  %s  %s\n", mark, name)
}

// firstNonEmpty returns the first non-empty string. Pure, localised.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return v
		}
	}
	return ""
}
