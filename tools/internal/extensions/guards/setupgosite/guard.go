package setupgosite

// guard.go implements `llz ci setup-go-sole-site` — the gate that keeps the Go
// toolchain setup defined in exactly ONE place.
//
// WHY: .github/actions/setup-llz exists because the setup-go block had been
// stamped out 13 times byte-identically, which made its pinned SHA a 13-site
// sweep to bump and let the build flags drift (4 of the 8 sites that followed it
// with a `go build` passed -buildvcs=false, 4 did not). The composite's own
// header records that, and .github/workflows/AGENTS.md states the rule it bought:
// "`actions/setup-go` should appear nowhere else."
//
// Nothing enforced it, and it had already broken. release-e2e-lane.yml's
// `llz functional` job carried a second copy pinned to v7.0.0 while the composite
// sat at v6.5.0 — a full major apart — in a job running the SAME functional
// script as llz-release.yml's `functional` job, which reaches it through the
// composite. So one release gate built its llz on one toolchain action and the
// other on another.
//
// NEITHER actionlint NOR any gate here could see it, and that is the point: every
// site is individually well-formed. A correctly SHA-pinned `uses:` is textually
// indistinguishable from the right one, so the defect is only visible as a
// RELATION between sites — which is the class this repo keeps rediscovering (see
// version-pins, whose subject is the same shape over tool versions).
//
// The rule is exemption-free by construction. instance-template/.github ships
// seven composite actions and no setup-go at all: an instance never builds llz
// from source, it consumes a released binary. So a hit anywhere outside the
// composite is a bypass, not a special case.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// soleSite is the one file allowed to reference the setup-go action. Slash-form;
// compared against filepath.ToSlash of the walked path.
const soleSite = ".github/actions/setup-llz/action.yml"

// scanRoots are the trees whose Actions YAML is checked. instance-template's is
// included deliberately: the rule must hold in what adopters carry too, and the
// cheapest way to keep it true there is to check it rather than to assume it.
var scanRoots = []string{".github", "instance-template/.github"}

// reSetupGo matches a `uses:` of the setup-go action, at any ref form.
//
// It is anchored on `uses:` rather than on the bare action name so that PROSE
// mentioning actions/setup-go in a YAML comment — which is how the composite's
// own header explains why it exists — is not reported as a second site. The
// distinction is the whole difference between a rule and a nuisance: the comment
// naming the thing is the documentation of the rule, not a violation of it.
var reSetupGo = regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*actions/setup-go@`)

// site is one file:line that reaches for the action.
type site struct {
	file string
	line int
	text string
}

// Run fails when any Actions YAML outside soleSite uses the setup-go action, and
// also when soleSite itself has stopped using it.
func Run(root string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)

	files, err := scanFiles(repo)
	if err != nil {
		return err
	}
	// FAIL CLOSED ON AN EMPTY CORPUS. A walk that found no Actions YAML is what a
	// wrong --root looks like, and reporting "OK — 0 files" over it would launder
	// a mis-invocation into a green check.
	if len(files) == 0 {
		return fmt.Errorf("setup-go-sole-site: no Actions YAML found under %s — refusing to pass vacuously",
			strings.Join(scanRoots, ", "))
	}

	var bypasses []site
	authority := 0
	for _, f := range files {
		data, err := repo.ReadFile(filepath.FromSlash(f))
		if err != nil {
			return fmt.Errorf("setup-go-sole-site: read %s: %w", f, err)
		}
		hits := findSites(f, string(data))
		if f == soleSite {
			authority += len(hits)
			continue
		}
		bypasses = append(bypasses, hits...)
	}

	// FAIL CLOSED ON A VANISHED AUTHORITY. Without this, deleting or renaming the
	// composite makes every bypass legal and the gate reports success having
	// checked that nothing violates a rule nothing states any more — the exact
	// "derive the expected set from the thing under test" trap.
	if authority == 0 {
		return fmt.Errorf("setup-go-sole-site: %s does not use actions/setup-go.\n"+
			"  That file is the one site allowed to, and every other workflow is held to it — so with it\n"+
			"  gone this gate would be enforcing a rule nothing declares. If the composite moved, update\n"+
			"  soleSite here and .github/workflows/AGENTS.md together", soleSite)
	}

	sort.Slice(bypasses, func(i, j int) bool {
		if bypasses[i].file != bypasses[j].file {
			return bypasses[i].file < bypasses[j].file
		}
		return bypasses[i].line < bypasses[j].line
	})

	if len(bypasses) == 0 {
		fmt.Fprintf(out, "setup-go-sole-site: OK — %d Actions file(s) scanned, setup-go used only in %s\n",
			len(files), soleSite)
		return nil
	}
	for _, s := range bypasses {
		fmt.Fprintf(errOut, "::error file=%s,line=%d::hand-rolled actions/setup-go — use ./.github/actions/setup-llz\n", s.file, s.line)
	}
	fmt.Fprintf(errOut, "\n%s %d workflow step(s) set up Go without %s:\n", color.Red("✗"), len(bypasses), soleSite)
	for _, s := range bypasses {
		fmt.Fprintf(errOut, "    %s:%d  %s\n", s.file, s.line, s.text)
	}
	fmt.Fprintf(errOut, "\nEach one is a second copy of the setup-go pin, which is how it drifted a major\n"+
		"version last time while both copies stayed valid. Replace the step with:\n\n"+
		"    - uses: ./.github/actions/setup-llz\n"+
		"      with:\n"+
		"        install-path: ${{ github.workspace }}/bin/llz   # omit to install the toolchain only\n\n"+
		"The one sanctioned hand-roll is llz-release.yml's `go build`, which stamps the\n"+
		"release version via -ldflags — it still takes its TOOLCHAIN from the composite.\n")
	return fmt.Errorf("setup-go-sole-site: %d step(s) bypass %s", len(bypasses), soleSite)
}

// findSites returns every setup-go `uses:` in body, with 1-based line numbers.
func findSites(file, body string) []site {
	var out []site
	for i, line := range strings.Split(body, "\n") {
		if reSetupGo.MatchString(line) {
			out = append(out, site{file: file, line: i + 1, text: strings.TrimSpace(line)})
		}
	}
	return out
}

// scanFiles walks scanRoots for Actions YAML, in slash form.
func scanFiles(repo capability.Repo) ([]string, error) {
	var files []string
	for _, r := range scanRoots {
		err := repo.WalkDir(filepath.FromSlash(r), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// An absent root is not a failure: this gate also runs in an instance
				// checkout, which has no instance-template/ of its own.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if n := d.Name(); strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
				files = append(files, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("setup-go-sole-site: walk %s: %w", r, err)
		}
	}
	sort.Strings(files)
	return files, nil
}
