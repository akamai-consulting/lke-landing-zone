package versionpins

// pippins.go — the second authority problem in dockerfiles/Dockerfile.
//
// The ARG block covers every tool that arrives as a downloaded binary, because
// `toolbox` fetches those once and each image COPYs them. The Python CLIs do not
// come that way: `uv pip install` runs INSIDE each stage that needs the tool, so
// the same package is pinned separately in ci-tofu and in devcontainer.
//
// THE FILE'S OWN HEADER IS THE CASE FOR THIS GATE. It records that every tool
// version used to be "pinned in 2-3 separate ARGs … with a 'kept in lockstep'
// comment and NOTHING enforcing it. A missed copy did not fail; it silently
// shipped a devcontainer whose helm differed from CI's." Consolidating the
// binaries fixed that for them and left the pip installs exactly as described —
// copier is pinned twice, and the only thing holding the two together is a
// comment saying they are pinned together on purpose.
//
// The skew is not cosmetic for copier specifically. ci-tofu's copy renders the
// template for the automated `llz upgrade` in llz-template-upgrade.yml, and the
// devcontainer's renders it for the `llz self-update && llz upgrade` an operator
// runs to reproduce the same diff. A major skew between them makes those two
// answers legitimately differ, and the PR that arrives is then unreproducible by
// the person reviewing it.
//
// NO AUTHORITY LINE, deliberately: unlike the ARG block there is no single
// declaration to be right, so the property gated is AGREEMENT rather than
// conformance. The first occurrence is reported as the expected one only so the
// message has something to name.

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// rePipPin matches one pinned requirement, e.g. "copier>=9.4,<10" or a bare
// copier>=9.4,<10. Captured as name + specifier.
//
// BOTH QUOTING STYLES, AND UNQUOTED — but only ever applied to the `pip install`
// lines isolated by pipInstallLines. Matching only double quotes gave the drift a
// place to hide that costs nothing to reach: re-quote one stage's requirement
// with single quotes and the scanner sees ONE pin for that package, which it can
// never compare against anything — err=nil, green, the two stages a major apart.
// Quoting style is a formatting choice and must not decide whether a pin is
// measured.
//
// APPLIED TO THE ISOLATED LINES RATHER THAN THE WHOLE FILE, which is the other
// half of the same lesson: relaxing the quotes over the file at large swept up
// `ARG TOFU_VERSION=1.12.5` and `--strip-components=1` as requirements — 22 pins
// where there are 5. The pattern has to be loose about quoting, so the REGION has
// to be tight.
//
// BY PATTERN, NOT BY A HAND-KEPT PACKAGE LIST — the same reason the ARG scan
// works that way: a list would have to be updated by the very commit that
// introduces the bug.
//
// WHAT THIS GATE DOES NOT CLAIM: that a package present in one stage and missing
// from the other is caught. It cannot be — linode-cli is legitimately
// devcontainer-only — so absence carries no signal and only DISAGREEMENT is
// gated. The one cross-stage presence that IS load-bearing, copier in both
// ci-tofu and devcontainer, is asserted directly in pippins_test.go.
var rePipPin = regexp.MustCompile(`["']?([A-Za-z][A-Za-z0-9._-]*)((?:[<>=!~]=?[0-9][^"'\s,]*)(?:,[<>=!~]=?[0-9][^"'\s,]*)*)["']?`)

// pipInstallLines returns the 1-based line numbers and text of every line that is
// part of a `pip install` command, following backslash continuations — the
// requirements in this repo are one per continued line.
func pipInstallLines(body string) map[int]string {
	out := map[int]string{}
	inCmd := false
	for i, l := range strings.Split(body, "\n") {
		if strings.Contains(l, "pip install") {
			inCmd = true
		}
		if inCmd {
			out[i+1] = l
		}
		if !strings.HasSuffix(strings.TrimRight(l, " \t"), `\`) {
			inCmd = false
		}
	}
	return out
}

// pipPin is one pinned requirement, and where it was written.
type pipPin struct {
	pkg, spec string
	line      int
}

// collectPipPins reads every pinned requirement on a pip install line, in file
// order.
func collectPipPins(repo capability.Repo) ([]pipPin, error) {
	data, err := repo.ReadFile(filepath.FromSlash(versionAuthorityFile))
	if err != nil {
		return nil, fmt.Errorf("version-pins: read %s: %w", versionAuthorityFile, err)
	}
	body := maskComments(versionAuthorityFile, string(data))
	lines := pipInstallLines(body)
	nums := make([]int, 0, len(lines))
	for n := range lines {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var pins []pipPin
	for _, n := range nums {
		for _, m := range rePipPin.FindAllStringSubmatch(lines[n], -1) {
			pins = append(pins, pipPin{pkg: m[1], spec: m[2], line: n})
		}
	}
	return pins, nil
}

// disagreeingPipPins returns, per package pinned more than once, every site whose
// specifier differs from the first one seen.
func disagreeingPipPins(pins []pipPin) []pinSite {
	// IDENTIFIED BY POSITION IN THE SCAN, NOT BY LINE. Using the line number made
	// two conflicting pins of one package on a SINGLE line compare equal to each
	// other and disappear — `pip install "copier>=9.4,<10" "copier>=10,<11"` passed.
	// A line is not a unique key for a pin; its index in the scan is.
	firstAt := map[string]int{}
	for i, p := range pins {
		if _, seen := firstAt[p.pkg]; !seen {
			firstAt[p.pkg] = i
		}
	}
	var bad []pinSite
	for i, p := range pins {
		f := pins[firstAt[p.pkg]]
		if i == firstAt[p.pkg] || p.spec == f.spec {
			continue
		}
		bad = append(bad, pinSite{
			file: versionAuthorityFile,
			line: p.line,
			what: fmt.Sprintf("pip pin %s%s", p.pkg, p.spec),
			got:  p.spec,
			want: f.spec,
		})
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].line < bad[j].line })
	return bad
}

// runPipPins reports the agreement check.
//
// FAILS CLOSED ON A BLIND SCAN, but only where blindness is provable: a
// Dockerfile that invokes `pip install` and yields no pins means the regex has
// stopped matching, and "all agree" over zero pins is indistinguishable from the
// drift this is meant to catch (docs/e2e-gates.md's vacuity archetype). A
// Dockerfile with no pip install at all legitimately has none, so the trigger is
// the INVOCATION rather than the count — keyed on the count alone, the doctrine
// reds every tree that simply does not use Python, which is what it did to five
// fixtures the first time it was written.
func runPipPins(repo capability.Repo, verbose bool, out, errOut io.Writer) (int, error) {
	pins, err := collectPipPins(repo)
	if err != nil {
		return 0, err
	}
	if len(pins) == 0 {
		installs, ierr := pipInstallsPresent(repo)
		if ierr != nil {
			return 0, ierr
		}
		if installs {
			e := fmt.Errorf("version-pins: %s runs `pip install` but the scan matched no pinned "+
				"requirement — it examined nothing, which reads exactly like agreement", versionAuthorityFile)
			fmt.Fprintf(errOut, "::error file=%s::%v\n", versionAuthorityFile, e)
			return 0, e
		}
		return 0, nil
	}
	if verbose {
		for _, p := range pins {
			fmt.Fprintf(out, "  pip   %s:%d  %s%s\n", versionAuthorityFile, p.line, p.pkg, p.spec)
		}
	}
	bad := disagreeingPipPins(pins)
	if len(bad) == 0 {
		return len(pins), nil
	}
	for _, s := range bad {
		fmt.Fprintf(errOut, "::error file=%s,line=%d::%s disagrees with the same package pinned as %s earlier "+
			"in this file\n", s.file, s.line, s.what, s.want)
	}
	fmt.Fprintf(errOut, "\n%s %d pip pin(s) in %s disagree across build stages:\n",
		color.Red("✗"), len(bad), versionAuthorityFile)
	for _, s := range bad {
		fmt.Fprintf(errOut, "    line %d  %s (the first pin of this package is %s)\n", s.line, s.what, s.want)
	}
	fmt.Fprintf(errOut, "\nA Python CLI is installed inside each stage that needs it, so the same package is\n"+
		"pinned once per stage and nothing but agreement makes them one version. For copier\n"+
		"the skew is load-bearing: ci-tofu's renders the template for the automated upgrade\n"+
		"in llz-template-upgrade.yml, the devcontainer's renders it for the local\n"+
		"`llz upgrade` an operator runs to reproduce that diff, and a major between them\n"+
		"makes the two legitimately differ. Bump them together.\n")
	return len(pins), fmt.Errorf("version-pins: %d pip pin(s) disagree", len(bad))
}

// pipPinSummary is the one line Run adds when the check passes.
func pipPinSummary(n int) string {
	return fmt.Sprintf("version-pins: OK — %d pip requirement(s) in %s agree across stages\n",
		n, strings.TrimSpace(versionAuthorityFile))
}

// pipInstallsPresent reports whether the Dockerfile installs anything with pip at
// all, which is what makes an empty pin scan evidence of a broken scanner rather
// than of a tree that uses no Python.
func pipInstallsPresent(repo capability.Repo) (bool, error) {
	data, err := repo.ReadFile(filepath.FromSlash(versionAuthorityFile))
	if err != nil {
		return false, fmt.Errorf("version-pins: read %s: %w", versionAuthorityFile, err)
	}
	return strings.Contains(maskComments(versionAuthorityFile, string(data)), "pip install"), nil
}
