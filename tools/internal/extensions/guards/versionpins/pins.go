package versionpins

// ci_version_pins.go implements `llz ci version-pins` — the consistency gate over
// tool-version pins.
//
// dockerfiles/Dockerfile declares itself "the single source of truth for tool
// versions". It is not: the same numbers are restated in the build matrix, in
// lint.yml's container fallbacks, in workflow env blocks, and in Go constants
// that derive TF_IMAGE/KUBE_IMAGE. Nothing checked them against each other.
//
// This has already gone wrong once, and the confession is still in the tree —
// tokens.go:
//
//	// A THIRD restatement of the image pin … It was still on 1.9.8 after both of
//	// those moved, which would have scaffolded new instances onto a HashiCorp
//	// Terraform image while every caller invoked `tofu`.
//
// That was caught by hand. This catches it in CI: the Dockerfile ARG block is the
// authority, every restatement must agree, and a restatement nobody knew about
// still gets checked because the scan is by pattern, not by a hand-kept file list.
//
// NOT ENFORCED HERE: digest-pinning the container fallbacks. `vars.TF_IMAGE` is
// the real pin and the `format(…ci-tofu:<ver>…)` fallback is a convenience
// default; requiring a digest there would churn on every image rebuild without
// making the pin more honest. Agreement is the property worth gating.

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

const versionAuthorityFile = "dockerfiles/Dockerfile"

// imagePin ties a published CI image to the Dockerfile ARG that drives its tag,
// and to the Go constant that restates it for TF_IMAGE/KUBE_IMAGE derivation.
// This table IS the mapping — declared once, read by every scanner below.
type imagePin struct {
	image   string // published image name, e.g. ci-tofu
	arg     string // Dockerfile ARG that sets its tag
	goConst string // constant in tools/cmd/llz that restates it ("" if none)
}

var imagePins = []imagePin{
	{image: "ci-tofu", arg: "TOFU_VERSION", goConst: "ciTofuTag"},
	{image: "ci-kubernetes", arg: "KUBECTL_VERSION", goConst: "ciKubernetesTag"},
}

// scanRoots are the trees a version restatement may legitimately live in.
//
// docs/ is deliberately absent: an ADR that says "CI runs 1.12.5" is a historical
// record of a decision, not a live pin. Gating it would force every ADR to be
// rewritten on every bump, which is both wrong and the fastest way to get a gate
// switched off.
var scanRoots = []string{
	".github",
	"instance-template/.github",
	"tools/cmd/llz",
	"dockerfiles",
	"template-scripts",
	"Makefile",
}

var (
	reDockerfileArg = regexp.MustCompile(`(?m)^ARG\s+([A-Z0-9_]+)=(\S+)`)
	// "image":"ci-tofu", … "version":"1.12.5" within one build-matrix JSON object.
	reMatrixEntry = regexp.MustCompile(`"image"\s*:\s*"([^"]+)"[^{}]*?"version"\s*:\s*"([^"]*)"`)
)

// reImageTag matches a published image reference carrying a version tag, e.g.
// ghcr.io/{0}/ci-tofu:1.12.5. The tag must start with a digit so a floating
// `:latest` or a `${{ … }}` expression is not mistaken for a pin.
func reImageTag(image string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(image) + `:([0-9][A-Za-z0-9._-]*)`)
}

// reArgRestatement matches `NAME: "1.2.3"` / `NAME=1.2.3` / `NAME := 1.2.3` for
// exactly NAME.
//
// The leading boundary is load-bearing. lint.yml carries ARGOCD_HELM_VERSION,
// ESO_HELM_VERSION and KYVERNO_HELM_VERSION — unrelated CHART versions that all
// end in the ARG name HELM_VERSION. A suffix match would report them as drift and
// the gate would be useless on day one.
//
// `:=` IS A SEPARATE ALTERNATIVE AND IT HAS TO COME FIRST. The separator was
// `[:=]` — one character — so it matched YAML's `:` and env/ARG's `=` and missed
// Make's `:=` entirely: after consuming the `:` the value had to start at `=`,
// which is not [0-9v], so the whole match failed and the site was silently not a
// site. `Makefile` is a scanRoot and the header above claims "a restatement
// nobody knew about still gets checked because the scan is by pattern" — but the
// Makefile's own `KUBECTL_VERSION := <version>`, the idiomatic Make form, was
// invisible to it. Demonstrated by setting a pin to a wrong value twice: written
// `=` the gate failed and named the line; written `:=` the same wrong value
// reported "OK — 9 restatements agree".
func reArgRestatement(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)(?:^|[^A-Z0-9_])` + regexp.QuoteMeta(name) + `\s*(?::=|[:=])\s*"?([0-9v][A-Za-z0-9._-]*)"?`)
}

func reGoConst(name string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(name) + `\s*=\s*"([^"]*)"`)
}

// pinSite is one place a version is restated.
type pinSite struct {
	file string
	line int
	what string // human-readable: what form the restatement took
	got  string
	want string
}

func (s pinSite) ok() bool { return s.got == s.want }

func Run(root string, verbose bool, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)
	args, err := loadVersionAuthority(repo)
	if err != nil {
		return err
	}
	files, err := versionScanFiles(repo)
	if err != nil {
		return err
	}
	sites, err := collectPinSites(repo, files, args)
	if err != nil {
		return err
	}

	var bad []pinSite
	for _, s := range sites {
		if !s.ok() {
			bad = append(bad, s)
		}
	}
	if verbose {
		for _, s := range sites {
			mark := "ok"
			if !s.ok() {
				mark = "DRIFT"
			}
			fmt.Fprintf(out, "  %-5s %s:%d  %s = %s\n", mark, s.file, s.line, s.what, s.got)
		}
	}
	if len(bad) == 0 {
		fmt.Fprintf(out, "version-pins: OK — %d restatement(s) agree with %s\n", len(sites), versionAuthorityFile)
		return nil
	}
	for _, s := range bad {
		fmt.Fprintf(errOut, "::error file=%s,line=%d::%s is %s but %s declares %s\n",
			s.file, s.line, s.what, s.got, versionAuthorityFile, s.want)
	}
	fmt.Fprintf(errOut, "\n%s %d version pin(s) disagree with %s:\n", color.Red("✗"), len(bad), versionAuthorityFile)
	for _, s := range bad {
		fmt.Fprintf(errOut, "    %s:%d  %s = %s (want %s)\n", s.file, s.line, s.what, s.got, s.want)
	}
	fmt.Fprintf(errOut, "\nThe Dockerfile ARG block is the authority. Either bump these to match it, or —\n"+
		"if the ARG is what is stale — bump the ARG and re-run. Every restatement of a\n"+
		"tool version must move together; that is the whole point of this gate.\n")
	return fmt.Errorf("version-pins: %d pin(s) drifted", len(bad))
}

// loadVersionAuthority reads the Dockerfile ARG block into name -> version.
func loadVersionAuthority(repo capability.Repo) (map[string]string, error) {
	data, err := repo.ReadFile(filepath.FromSlash(versionAuthorityFile))
	if err != nil {
		return nil, fmt.Errorf("version-pins: read %s: %w", versionAuthorityFile, err)
	}
	args := map[string]string{}
	for _, m := range reDockerfileArg.FindAllStringSubmatch(string(data), -1) {
		args[m[1]] = m[2]
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("version-pins: %s declares no ARG versions — refusing to pass vacuously", versionAuthorityFile)
	}
	return args, nil
}

// versionScanFiles walks scanRoots for text files that may restate a version.
// Test files are excluded: a fixture legitimately pins a made-up version.
func versionScanFiles(repo capability.Repo) ([]string, error) {
	var files []string
	for _, r := range scanRoots {
		start := filepath.FromSlash(r)
		err := repo.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // an optional root (Makefile, template-scripts) may be absent
				}
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", ".terraform", "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, "_test.go") {
				return nil
			}
			switch {
			case strings.HasSuffix(name, ".yml"), strings.HasSuffix(name, ".yaml"),
				strings.HasSuffix(name, ".go"), strings.HasSuffix(name, ".sh"),
				strings.HasPrefix(name, "Dockerfile"), name == "Makefile":
				// Already repo-relative: the reader expresses everything under
				// its own root.
				files = append(files, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("version-pins: walk %s: %w", r, err)
		}
	}
	sort.Strings(files)
	return files, nil
}

func collectPinSites(repo capability.Repo, files []string, args map[string]string) ([]pinSite, error) {
	var sites []pinSite
	for _, rel := range files {
		data, err := repo.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return nil, fmt.Errorf("version-pins: read %s: %w", rel, err)
		}
		body := maskComments(rel, string(data))
		isAuthority := rel == versionAuthorityFile

		// 1. Published image tags: ghcr.io/<org>/ci-tofu:1.12.5
		for _, p := range imagePins {
			want, ok := args[p.arg]
			if !ok {
				continue
			}
			for _, m := range reImageTag(p.image).FindAllStringSubmatchIndex(body, -1) {
				sites = append(sites, pinSite{
					file: rel, line: lineOf(body, m[0]),
					what: p.image + " image tag",
					got:  body[m[2]:m[3]], want: want,
				})
			}
			// 3. Go constant restating the tag.
			if p.goConst != "" {
				for _, m := range reGoConst(p.goConst).FindAllStringSubmatchIndex(body, -1) {
					sites = append(sites, pinSite{
						file: rel, line: lineOf(body, m[0]),
						what: p.goConst,
						got:  body[m[2]:m[3]], want: want,
					})
				}
			}
		}

		// 2. build-images.yml matrix: {"image":"ci-tofu", … "version":"1.12.5"}.
		for _, m := range reMatrixEntry.FindAllStringSubmatchIndex(body, -1) {
			image, ver := body[m[2]:m[3]], body[m[4]:m[5]]
			if ver == "" {
				continue // devcontainer/llz carry no version tag
			}
			for _, p := range imagePins {
				if p.image != image {
					continue
				}
				if want, ok := args[p.arg]; ok {
					sites = append(sites, pinSite{
						file: rel, line: lineOf(body, m[0]),
						what: "build matrix version for " + image,
						got:  ver, want: want,
					})
				}
			}
		}

		// 4. Bare ARG restatements: KUBECTL_VERSION: "<version>".
		if isAuthority {
			continue // the ARG lines themselves are the authority, not restatements
		}
		for _, name := range sortedKeys(args) {
			for _, m := range reArgRestatement(name).FindAllStringSubmatchIndex(body, -1) {
				sites = append(sites, pinSite{
					file: rel, line: lineOf(body, m[0]),
					what: name,
					got:  body[m[2]:m[3]], want: args[name],
				})
			}
		}
	}
	return sites, nil
}

func lineOf(body string, off int) int {
	return 1 + strings.Count(body[:off], "\n")
}

// maskComments blanks out whole-line comments, preserving length so byte offsets
// and line numbers still line up with the original.
//
// A version inside a comment is prose, not a pin — the same reasoning that keeps
// docs/ out of scanRoots. Without this the gate reports its own header (which
// documents the `ci-tofu:<ver>` form it matches) and any workflow comment that
// mentions a version in passing.
func maskComments(rel, body string) string {
	marker := "#"
	if strings.HasSuffix(rel, ".go") {
		marker = "//"
	}
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), marker) {
			lines[i] = strings.Repeat(" ", len(ln))
		}
	}
	return strings.Join(lines, "\n")
}
