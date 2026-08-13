package cli

// version_stamp_test.go — every build that stamps a version must name a variable
// that exists.
//
// ────────────────────────────────────────────────────────────────────────────
// A WRONG -X DOES NOT FAIL THE BUILD, WHICH IS THE WHOLE PROBLEM.
//
// The linker silently ignores `-X path.Name=value` for a symbol it cannot
// resolve. So when the command tree moved from package main to internal/cli, the
// release workflow's `-X main.version=${VER}` did not break — it would have kept
// building, kept publishing, and shipped every binary reporting "dev".
//
// That failure is invisible at the point it happens and expensive later:
// selfupgrade compares this string against the newest release to decide whether an
// update is AVAILABLE, so a permanently-"dev" binary sees every release as newer
// than itself, forever. copier anchors a scaffold to it, and templatecommit pins an
// adopter to it.
//
// It is exactly the shape AGENTS.md names — "assert at the CONSUMER on data the
// producer really emitted" — so this reads the ACTUAL build files and checks the
// symbol against the ACTUAL package, rather than restating either.
//
// IT ORIGINALLY READ ONLY llz-release.yml, AND THE OTHER SITE SHIPPED BROKEN.
// The move that prompted this test updated the release workflow and left
// dockerfiles/Dockerfile stamping `main.version`. So the release binaries were
// stamped correctly while every ci-tofu/ci-kubernetes image baked an llz
// reporting "dev" — and `llz ci assert-image-fresh`, which compares that stamp
// against the instance's template pin, degraded to warn-and-pass on every
// adopter. A guard that covers one of two producers reports green on the one it
// does not read, which is worse than no guard: it was cited as the reason the
// symbol could not drift. So this now checks EVERY build file, and Step 3 below
// fails when a new stamping site appears that this list does not name.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const repoRoot = "../../.."

// stampingBuilds are the builds whose output is CONSUMED as a version — each one
// must carry a correct `-X`. Being on this list means "stamping here is
// load-bearing", not merely "this file builds llz": the many unstamped
// `go build ./cmd/llz` sites (setup-llz, the template-scripts) produce
// throwaway CI binaries whose version nothing reads.
var stampingBuilds = []struct {
	path string
	why  string // what reads the stamp, and what breaks when it is absent
}{
	{
		".github/workflows/llz-release.yml",
		"selfupgrade compares this against the newest release; an unstamped binary sees every release as newer than itself, forever",
	},
	{
		"dockerfiles/Dockerfile",
		"`llz ci assert-image-fresh` reads the baked stamp to compare the ci image against the instance's template pin; unstamped, it warns and passes and the skew guard is off",
	},
}

// ldflagX matches a version ldflag, capturing the symbol path. The symbol must
// contain a dot (`pkg.Name`), which is what separates a real `-X` from `curl -X
// POST` elsewhere in the tree. The value is deliberately unconstrained — each
// site names its own build variable (${VER}, ${LLZ_VERSION}) and which one is
// correct is not this test's business; that the symbol resolves is.
var ldflagX = regexp.MustCompile(`-X ([\w./-]+\.\w+)=(\S+?)"`)

// buildsLLZ matches a `go build` of the CLI, used to find stamping sites this
// test does not yet know about.
var buildsLLZ = regexp.MustCompile(`go build[^\n]*\./cmd/llz`)

func TestEveryStampingBuildNamesThisPackagesVersionVar(t *testing.T) {
	want := versionSymbol()

	// ── Step 1: every listed site stamps, and stamps the right symbol ──
	for _, b := range stampingBuilds {
		body, err := os.ReadFile(filepath.FromSlash(filepath.Join(repoRoot, b.path)))
		if err != nil {
			t.Errorf("%s stamps this binary and must exist: %v", b.path, err)
			continue
		}

		m := ldflagX.FindAllStringSubmatch(string(body), -1)
		if len(m) == 0 {
			t.Errorf("%s passes no `-X <symbol>=<value>` ldflag, so the binary it builds reports the "+
				"default version %q.\n\tThis matters here because %s.", b.path, Version, b.why)
			continue
		}
		for _, got := range m {
			if got[1] != want {
				t.Errorf("%s stamps %q, but the version variable is %q.\n"+
					"\tThe linker IGNORES an -X for a symbol it cannot resolve, so this does NOT fail the "+
					"build — it silently ships binaries reporting the default version forever.\n"+
					"\tHere that means: %s.",
					b.path, got[1], want, b.why)
			}
		}
	}

	// ── Step 2: fail closed on vacuity ──
	// A list that resolved to nothing — every path renamed, the regex outgrown by
	// a reformatted ldflag — would pass Step 1 by examining zero symbols.
	if t.Failed() {
		return // Step 1 already said why; do not pile on.
	}
	if len(stampingBuilds) == 0 {
		t.Fatal("stampingBuilds is empty: this test would examine nothing and report green")
	}

	// ── Step 3: no stamping site escapes the list ──
	// The original failure was not a wrong symbol anyone typed twice — it was a
	// SECOND build site that the guard did not read. Any file that builds
	// ./cmd/llz *with* an -X ldflag is a stamping site, so it must be listed
	// above with the reason its stamp is load-bearing.
	listed := map[string]bool{}
	for _, b := range stampingBuilds {
		listed[filepath.FromSlash(b.path)] = true
	}
	for _, path := range buildFiles(t) {
		if listed[path] {
			continue
		}
		body, err := os.ReadFile(filepath.Join(repoRoot, path))
		if err != nil {
			continue
		}
		text := string(body)
		if !buildsLLZ.MatchString(text) {
			continue
		}
		if m := ldflagX.FindStringSubmatch(text); m != nil {
			t.Errorf("%s builds ./cmd/llz and stamps %q, but is not in stampingBuilds — "+
				"so nothing checks that symbol against the real variable %q.\n"+
				"\tAdd it to the list (with what reads its stamp), or drop the -X if the version "+
				"of that binary is never read.", path, m[1], want)
		}
	}
}

// The variable must also be EXPORTED and a string, which is the other half of what
// -X requires: the linker refuses a non-string target, and cannot reach an
// unexported one from outside its package.
func TestVersionVarIsStampable(t *testing.T) {
	if reflect.TypeOf(Version).Kind() != reflect.String {
		t.Errorf("Version is %s; -X can only stamp a string", reflect.TypeOf(Version).Kind())
	}
	if Version == "" {
		t.Error("Version defaults to empty — an unstamped binary should still say something, " +
			"and `llz version` printing nothing reads as a broken install rather than a dev build")
	}
}

// versionSymbol is the fully-qualified name -X must target. The package path is
// DERIVED, not written down: if this package moves again, the test moves with it
// and keeps checking the real thing. Any func in the package will do — runtime
// spells its name as "<pkgpath>.<Func>".
func versionSymbol() string {
	full := runtime.FuncForPC(reflect.ValueOf(Main).Pointer()).Name()
	return full[:strings.LastIndex(full, ".")] + ".Version"
}

// buildFiles lists the repo files that can define a build, relative to repoRoot.
// It walks rather than globbing a fixed set so a stamping site added in a new
// workflow or a new Dockerfile is seen without editing this test.
func buildFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	skip := map[string]bool{".git": true, "node_modules": true, "vendor": true, "testdata": true}
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not this test's concern
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		ext := filepath.Ext(name)
		if ext != ".yml" && ext != ".yaml" && ext != ".sh" &&
			name != "Makefile" && !strings.HasPrefix(name, "Dockerfile") && ext != ".mk" {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("cannot walk the repo for build files, so the completeness check would examine nothing: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("found no build files at all — the walk is broken and this check is vacuous")
	}
	return out
}
