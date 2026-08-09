package registry

// commands_census_test.go — the wiring table must name EVERY cobra constructor the
// extensions expose, and this derives both sides rather than trusting either.
//
// ────────────────────────────────────────────────────────────────────────────
// THE GUARD HAD A TWELVE-PACKAGE BLIND SPOT, AND THAT IS WHY THIS EXISTS.
//
// commands.go's whole stated purpose is that "an extension could be declared,
// validate, appear in `llz extension list`, and still have a verb nobody could
// run". TestEveryExtensionCommandIsWiredIn checks exactly that — for the
// constructors someone remembered to add to the table. A census found 158 exported
// constructors under internal/extensions and 139 rows, with twelve packages absent
// outright:
//
//	assertsuite  cosignguard  coverageguard  firewall  meshegress  monitoringlabel
//	mtlsguard    pincoherence reconciler     templatemanifest  versionpins  workflowshells
//
// All twelve WERE wired into the tree, so nothing was broken. The hole was in the
// guard: it could not fail for a command it had never heard of. That is the same
// shape as the prose version of undrivenGates, which claimed six driven gates when
// there were thirteen — a hand-maintained list covering what someone remembered.
//
// The packages it missed are not random. Every one of them exports a plainly-named
// `Cmd()` rather than `SomethingCmd()`, which is the convention the later guard
// conversions settled on. So the table's coverage tracked an accident of naming.
//
// BOTH SIDES ARE DERIVED. The left is go/ast over the extension tree — what
// EXISTS. The right is runtime reflection over the `commands` table — what is
// LISTED. Neither is transcribed, so neither can drift: a new constructor appears
// on the left the moment it compiles, and the only way to satisfy this test is to
// list it or to exempt it with an argument.
// ────────────────────────────────────────────────────────────────────────────

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// censusExempt names constructors that legitimately need no row, with the reason.
//
// A BOOL WOULD NOT HAVE BEEN ENOUGH. An exemption without an argument is
// indistinguishable from an oversight, which is the distinction this whole file
// exists to draw.
var censusExempt = map[string]string{
	// The `llz credentials` family is a tree: CredentialsCmd is listed, and these
	// are its children, attached by their own parent rather than by an AddCommand
	// in the CLI. Listing them would assert they are reachable at top level, which
	// they are not and should not be.
	"credrotate.CredentialsPATCmd":             "child of credrotate.CredentialsCmd, which is listed",
	"credrotate.CredentialsPATCreateCmd":       "grandchild of credrotate.CredentialsCmd, which is listed",
	"credrotate.CredentialsPATRevokeOldCmd":    "grandchild of credrotate.CredentialsCmd, which is listed",
	"credrotate.CredentialsObjKeyCmd":          "child of credrotate.CredentialsCmd, which is listed",
	"credrotate.CredentialsObjKeyCreateCmd":    "grandchild of credrotate.CredentialsCmd, which is listed",
	"credrotate.CredentialsObjKeyRevokeOldCmd": "grandchild of credrotate.CredentialsCmd, which is listed",
	"credrotate.CredentialsLKEAdminCmd":        "child of credrotate.CredentialsCmd, which is listed",

	// The SAME verb as docsguard.DocsGuardCmd, which is listed, differing only in
	// receiving the live cobra tree as data (see Gate.NewWithTree — a parentless
	// docs-guard resolves every documented invocation against a tree of one). Two
	// rows for one verb would make TestEveryExtensionCommandIsWiredIn assert the
	// same name twice and report coverage it does not have.
	"docsguard.DocsGuardCmdFor": "the tree-aware constructor for docs-guard, whose plain form is listed",
}

func TestWiringTableNamesEveryExposedConstructor(t *testing.T) {
	exposed := censusConstructors(t)
	if len(exposed) < 100 {
		t.Fatalf("the census found only %d constructors under the extension tree — it is reading "+
			"the wrong directory or its signature match broke, and a census that finds nothing "+
			"agrees with any table", len(exposed))
	}
	listed := listedConstructors(t)

	var missing []string
	for _, name := range exposed {
		if listed[name] {
			continue
		}
		if _, ok := censusExempt[name]; ok {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d extension constructor(s) are exposed but named by no row in commands.go:\n\t%s\n"+
			"\tThe wiring guard cannot fail for a command it has never heard of, so an "+
			"unlisted constructor is one TestEveryExtensionCommandIsWiredIn silently does not "+
			"cover. Add a row, or add it to censusExempt with the reason it needs none.",
			len(missing), strings.Join(missing, "\n\t"))
	}

	// The ratchet half. An exemption for a constructor that is now listed — or that
	// no longer exists — is a stale allowance, and a stale allowance is what the
	// next constructor gets written to match.
	have := map[string]bool{}
	for _, n := range exposed {
		have[n] = true
	}
	var stale []string
	for name, why := range censusExempt {
		if !have[name] || listed[name] {
			stale = append(stale, name)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("censusExempt[%q] has no reason", name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d exemption(s) no longer apply — DELETE them from censusExempt:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}

	t.Logf("census: %d constructor(s) exposed, %d listed, %d exempt", len(exposed), len(listed), len(censusExempt))
}

// censusConstructors parses the extension tree and returns every exported function
// returning a *cobra.Command, as "package.Func".
//
// IT MATCHES THE RETURN TYPE, NOT THE NAME. The blind spot this test closes was
// caused by a name convention (`Cmd` vs `SomethingCmd`), so keying on the name
// would rebuild the hole in the check for it. Parameters are ignored: several
// constructors take one (docsguard.DocsGuardCmdFor takes the cobra tree), and what
// makes something a command constructor is what it hands back.
func censusConstructors(t *testing.T) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()

	// extensionsDir walks up for the tree rather than hard-coding a depth — this
	// package has already moved once and a relative literal pointed at nothing
	// afterwards. Shared with grants_back_capabilities_test.go for that reason.
	dir := extensionsDir(t)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		pkg := f.Name.Name
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			// A method is not a package-level constructor; the CLI can only call
			// the latter.
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if returnsCobraCommand(fn) {
				out = append(out, pkg+"."+fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// returnsCobraCommand reports whether fn's sole result is *cobra.Command.
func returnsCobraCommand(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return false
	}
	star, ok := fn.Type.Results.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Command" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "cobra"
}

// listedConstructors reads the `commands` table through reflection, so the right
// side of the comparison is the real table rather than a parse of its source. A row
// that stopped compiling would not be here to be counted.
func listedConstructors(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, c := range Commands() {
		full := runtime.FuncForPC(reflect.ValueOf(c.New).Pointer()).Name()
		out[shortFuncName(full)] = true
	}
	return out
}

// shortFuncName turns a fully-qualified runtime name into "package.Func".
//
// The last path element holds both, e.g.
// ".../extensions/guards/meshegress.Cmd" → "meshegress.Cmd".
func shortFuncName(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		full = full[i+1:]
	}
	// A constructor referenced as a value keeps its own name; -fm would mean a
	// method value, which the table does not use.
	return strings.TrimSuffix(full, "-fm")
}
