package cli

// incluster_wiring_test.go — a seam an IN-CLUSTER workload reaches must not be
// wired to something that shells out.
//
// THE DEFECT, AND WHY IT IS A WIRING BUG RATHER THAN A CODE BUG. The `llz` image
// is `gcr.io/distroless/static-debian12:nonroot`: the llz binary and nothing
// else. No gh, no kubectl, no shell. Six workloads in platform-apl/ run it.
//
// The in-cluster PACKAGES are clean — reconciler, credrotate, harbor, tokeninv
// and healthsla contain zero exec.Command between them. The bug was one hop away,
// in the composition root: internal/cli wired credrotate's secret-writer seam to
// `ghsecret.SetFn`, which is `ghsecret.Set`, which is `exec.Command("gh", …)`.
//
// So the broad-PAT rotator could never publish. Every rotation returned
// "executable file not found", and because the error formatter dropped `err` it
// printed as a bare colon — it took a live cluster and a session of remote poking
// to find. The native writer it should have used (ghsecret.SetEnvNative, REST via
// forge.GitHubSecretWriter) already existed; nobody had pointed the wiring at it.
//
// Nothing could see it: the package is clean, the seam is legal, and the wiring
// compiles. Only the RELATION between "this verb runs in a distroless image" and
// "this implementation forks a process" is wrong, and that relation is exactly
// what this test computes.
//
// SCOPE, stated honestly. Detection is one hop plus var-aliases: a symbol counts
// as shelling out if its own body calls exec.Command, or if it is a package-level
// var initialised to such a function (which is precisely the SetFn -> Set -> exec
// shape). A longer chain would slip through. That is a real limit, not a claim of
// completeness — but it covers every seam wired in this package today, and the
// alternative (a full call graph) buys little for the surface involved.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// llzImageMarker identifies a container running the llz image. Matching on the
// repository segment rather than a full ref so an adopter's fork and a sha- tag
// are both recognised.
const llzImageMarker = "/llz:"

// inClusterEntrypoints returns the argv of every container in platform-apl/ that
// runs the llz image, e.g. ["ci","rotate-broad-pat","--apply"].
func inClusterEntrypoints(t *testing.T, platformAPL string) [][]string {
	t.Helper()
	var out [][]string
	var walk func(n any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if img, ok := v["image"].(string); ok && strings.Contains(img, llzImageMarker) {
				var argv []string
				for _, key := range []string{"command", "args"} {
					if raw, ok := v[key].([]any); ok {
						for _, a := range raw {
							if s, ok := a.(string); ok {
								argv = append(argv, s)
							}
						}
					}
				}
				if len(argv) > 0 {
					out = append(out, argv)
				}
			}
			for _, x := range v {
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	err := filepath.Walk(platformAPL, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !(strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		for {
			var doc any
			if derr := dec.Decode(&doc); derr != nil {
				break // end of stream, or a template this test need not parse
			}
			walk(doc)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", platformAPL, err)
	}
	return out
}

// verbOf reduces an argv to its command path, dropping an absolute binary path
// and every flag: ["/usr/local/bin/llz","ci","health-incluster"] -> "health-incluster".
func verbOf(argv []string) string {
	var words []string
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			break
		}
		if strings.Contains(a, "/") { // the binary itself
			continue
		}
		words = append(words, a)
	}
	if len(words) == 0 {
		return ""
	}
	return words[len(words)-1]
}

// shellingSymbols returns "pkg.Symbol" for every exported symbol under root whose
// body calls exec.Command, plus package-level vars aliased to one of them.
func shellingSymbols(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	type pending struct{ pkg, name, target string }
	var aliases []pending

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == "testdata" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, src, 0)
		if perr != nil {
			return perr
		}
		pkg := f.Name.Name

		execIn := func(n ast.Node) bool {
			found := false
			ast.Inspect(n, func(x ast.Node) bool {
				sel, ok := x.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Command" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "exec" {
					found = true
				}
				return true
			})
			return found
		}

		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Name.IsExported() && decl.Body != nil && execIn(decl.Body) {
					out[pkg+"."+decl.Name.Name] = true
				}
			case *ast.GenDecl:
				if decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, nm := range vs.Names {
						if !nm.IsExported() || i >= len(vs.Values) {
							continue
						}
						switch val := vs.Values[i].(type) {
						case *ast.FuncLit:
							if execIn(val.Body) {
								out[pkg+"."+nm.Name] = true
							}
						case *ast.Ident: // var SetFn = Set  — resolve after the walk
							aliases = append(aliases, pending{pkg, nm.Name, val.Name})
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	// Resolve one level of aliasing: `var SetFn = Set` inherits Set's verdict.
	// This is the exact shape the broad-PAT bug took.
	for _, a := range aliases {
		if out[a.pkg+"."+a.target] {
			out[a.pkg+"."+a.name] = true
		}
	}
	return out
}

func TestInClusterVerbsAreWiredNative(t *testing.T) {
	const platformAPL = "../../../platform-apl"
	if _, err := os.Stat(platformAPL); err != nil {
		t.Skipf("no platform-apl tree at %s", platformAPL)
	}

	entrypoints := inClusterEntrypoints(t, platformAPL)
	if len(entrypoints) == 0 {
		t.Fatal("found no container running the llz image in platform-apl/ — refusing to pass " +
			"vacuously; if the in-cluster workloads moved, point this at their new home")
	}
	verbs := map[string]bool{}
	for _, e := range entrypoints {
		if v := verbOf(e); v != "" {
			verbs[v] = true
		}
	}
	t.Logf("in-cluster entrypoints: %d, distinct verbs: %d", len(entrypoints), len(verbs))

	shells := shellingSymbols(t, "../..")
	if len(shells) == 0 {
		t.Fatal("found no symbol that calls exec.Command — the scan examined nothing")
	}

	// Every Deps/Install wiring in this package, as `pkg.Symbol` references.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), e.Name(), nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", e.Name(), perr)
		}
		// local alias -> import path tail, so `sharedopenbao.X` resolves to openbao.X
		local := map[string]string{}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			tail := path[strings.LastIndex(path, "/")+1:]
			name := tail
			if imp.Name != nil {
				name = imp.Name.Name
			}
			local[name] = tail
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Install" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			target := local[id.Name]
			if target == "" {
				target = id.Name
			}
			if !isInClusterPkg(target, verbs) {
				return true
			}
			checked++
			// Any pkg.Symbol referenced inside this Install call.
			ast.Inspect(call, func(x ast.Node) bool {
				s, ok := x.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				xid, ok := s.X.(*ast.Ident)
				if !ok {
					return true
				}
				p := local[xid.Name]
				if p == "" {
					p = xid.Name
				}
				if shells[p+"."+s.Sel.Name] {
					t.Errorf("%s: %s runs IN-CLUSTER (distroless llz image: the binary and nothing "+
						"else) but its Install wires %s.%s, which calls exec.Command.\n"+
						"\tThat process does not exist in the image, so the seam fails on every call —\n"+
						"\tthe broad-PAT rotator published to nowhere for exactly this reason. Wire the\n"+
						"\tnative implementation (e.g. ghsecret.SetEnvNative over the REST API) instead.",
						e.Name(), target, p, s.Sel.Name)
				}
				return true
			})
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no Install call for an in-cluster package was found in internal/cli — either the " +
			"wiring moved or the verb->package mapping below is stale; both make this test vacuous")
	}
	t.Logf("checked %d in-cluster Install wiring(s) against %d shelling symbol(s)", checked, len(shells))
}

// isInClusterPkg maps a wired package name onto the in-cluster verb set.
//
// The mapping is BY PACKAGE NAME against the verbs the manifests actually run,
// so it is derived from platform-apl rather than hand-kept: `ci rotate-broad-pat`
// is credrotate's, `ci token-inventory` is tokeninv's, and so on. A package whose
// name does not appear is not reached in-cluster and is not this test's business.
func isInClusterPkg(pkg string, verbs map[string]bool) bool {
	byPkg := map[string][]string{
		"credrotate": {"rotate-broad-pat"},
		"tokeninv":   {"token-inventory"},
		"harbor":     {"harbor-provisioner"},
		"reconciler": {"reconcile"},
		"converge":   {"health-incluster"},
		"objproxy":   {"obj-proxy"},
		"openbao":    {"bao-seed-all"},
	}
	for _, v := range byPkg[pkg] {
		if verbs[v] {
			return true
		}
	}
	return false
}
