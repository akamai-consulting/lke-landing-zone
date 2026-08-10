package cli

// version_stamp_test.go — the release ldflag must name a variable that exists.
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
// producer really emitted" — so this reads the ACTUAL workflow file and checks the
// symbol against the ACTUAL package, rather than restating either.
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

const releaseWorkflow = "../../../.github/workflows/llz-release.yml"

// ldflagX matches the -X the release build passes, capturing the symbol path.
var ldflagX = regexp.MustCompile(`-X ([^\s=]+)=\$\{VER\}`)

func TestReleaseLdflagNamesThisPackagesVersionVar(t *testing.T) {
	body, err := os.ReadFile(filepath.FromSlash(releaseWorkflow))
	if err != nil {
		t.Fatalf("the release workflow is what stamps this binary and must exist: %v", err)
	}

	m := ldflagX.FindAllStringSubmatch(string(body), -1)
	if len(m) == 0 {
		t.Fatal("llz-release.yml passes no `-X <symbol>=${VER}` — the release binaries would " +
			"report the default version, and selfupgrade would see every release as newer " +
			"than the running binary")
	}
	if len(m) > 1 {
		t.Fatalf("llz-release.yml passes %d version ldflags; this test assumes one and would "+
			"check only the first", len(m))
	}
	got := m[0][1]

	// The package path is DERIVED, not written down: if this package moves again,
	// the test moves with it and keeps checking the real thing. Any func in the
	// package will do — runtime spells its name as "<pkgpath>.<Func>".
	full := runtime.FuncForPC(reflect.ValueOf(Main).Pointer()).Name()
	pkgPath := full[:strings.LastIndex(full, ".")]
	want := pkgPath + ".Version"

	if got != want {
		t.Errorf("llz-release.yml stamps %q, but the version variable is %q.\n"+
			"\tThe linker IGNORES an -X for a symbol it cannot resolve, so this does not fail "+
			"the release build — it ships binaries that report the default version forever.",
			got, want)
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
