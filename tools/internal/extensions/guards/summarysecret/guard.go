package summarysecret

// guard.go implements `llz ci summary-secret-guard` — the static gate on
// SECRET MATERIAL REACHING $GITHUB_STEP_SUMMARY.
//
// THE SCAR. `llz ci bao-init` masked the OpenBao root token and all five
// recovery shares, then wrote the raw `bao operator init` payload — those same
// six values — into a fenced block in the job summary. The mask three lines
// above is what made the append look reviewed. It is not the same channel:
// ghsecret.Mask redacts the LOG stream, while a job summary is a Markdown file
// GitHub renders exactly as written, and Actions READ is enough to open it.
//
// THE RULE. In any FILE that calls ghsecret.Mask, every argument to a
// $GITHUB_STEP_SUMMARY append must be a string LITERAL, or be registered below.
//
// THE UNIT IS THE FILE, not the function, because extracting a helper would
// otherwise evade it — moving an append one function away from the Mask call is
// an ordinary refactor, and it would empty this guard's corpus for the file it
// exists to watch. Chasing the call graph instead needs a type-checked load and
// still loses the value across a package boundary.
//
// WHY NOT "no secrets in the summary": that needs dataflow analysis, and a guard
// guessing whether an expression is secret is wrong in both directions. The Mask
// call is a marker the author planted — "this code holds something that must not
// be printed" — so scoping the strict rule to those files is what makes it cheap.
// Measured here: 86 summary appends across 45 files, four of which also mask, so
// the registry is six call sites rather than eighty-six.
//
// WHAT IT DOES NOT DO. It cannot see a secret that reaches the summary from a
// function which never masks — a value read straight from os.Getenv and printed,
// say. That residue is real and belongs to review; it is stated here so nobody
// reads a green run as "no secret can reach a job summary". It also does not
// look at workflow YAML: `echo "$SECRET" >> $GITHUB_STEP_SUMMARY` in a `run:`
// block is invisible to it, and untestable-loc-check is what pushes that logic
// into Go where this can see it.
//
// UNUSED ENTRIES FAIL, for the reason plaintextAllowed's do: a registry keeping
// entries for call sites that no longer exist stops being reviewable, because
// the next reader cannot tell which lines are load-bearing.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"
)

// summaryRule is why one computed value is allowed into a job summary written by
// a function that also handles secrets.
type summaryRule struct {
	// reason must say WHAT the expression evaluates to and why reading it costs
	// nothing — "the ciphertext, useless without the operator's offline private
	// key" is reviewable; "not a secret" is not.
	reason string
}

// summaryComputedAllowed registers every computed argument to a
// $GITHUB_STEP_SUMMARY append inside a masking FILE, keyed by
// "<file-relative-path>:<function>" — file scope for what is checked, function
// granularity for what is reviewed.
//
// Keyed by FUNCTION, not by line: a line number churns on every edit above it,
// and the reviewer question ("is this function allowed to compute into the
// summary?") is a per-function one. The cost is that a second, genuinely unsafe
// append added to an already-registered function inherits its exemption — so a
// reason that covers the whole function is the bar, and a function that grows a
// second, differently-justified append should be split.
var summaryComputedAllowed = map[string]summaryRule{
	"tools/internal/extensions/lifecycle/openbao/ci_bao_breakglass.go:breakglassEncryptAndDeliver": {
		reason: "the RSA-OAEP/SHA-256 CIPHERTEXT of the break-glass root token, plus the region and " +
			"the dispatching actor. Delivering the ciphertext through the summary is the whole design " +
			"of that lane — it is unreadable without the operator's offline private key, which is what " +
			"makes the summary an acceptable channel for it",
	},
	"tools/internal/extensions/lifecycle/openbao/ci_bao_breakglass.go:BreakglassDeleteStored": {
		reason: "the infra-<region> environment NAME and the dispatching actor. No key material: this " +
			"append reports whether a secret was DELETED, and the outcome string is chosen from two " +
			"literals by whether the delete succeeded",
	},
	"tools/internal/extensions/lifecycle/openbao/ci_openbao_init.go:deliverEscrowedShares": {
		reason: "the RSA-OAEP/SHA-256 ciphertext of the 5 recovery shares, one block each, plus the " +
			"region name. Inline in the summary on purpose: the artifact upload is a separate step a " +
			"caller can omit, and the shares are minted exactly once",
	},
	"tools/internal/extensions/lifecycle/openbao/ci_openbao_init.go:appendInitSummary": {
		reason: "the region name, interpolated into the operator's next-steps banner (which " +
			"infra-<region> environment holds the shares, which secret to delete). Carries no value " +
			"derived from the init payload on either path",
	},
	"tools/internal/extensions/lifecycle/openbao/ci_bao_ensure_ready.go:RunEnsureReady": {
		reason: "the region name, in the 'root token unavailable, configure/seed skipped' notice. The " +
			"token itself is masked and never appended — this branch runs precisely when there is no " +
			"token to hold",
	},
	"tools/internal/extensions/lifecycle/harbor/harbor.go:harborAPI.createRobot": {
		reason: "the Harbor robot's NAME on the 409-already-exists path, which carries no secret " +
			"material (same reasoning as credCoverageExempt's not-a-credential class). The robot " +
			"SECRET is masked and returned to the caller, never appended",
	},
}

// finding is one computed argument to a summary append inside a masking file.
type finding struct {
	file, fn string
	line     int
	expr     string
}

func (f finding) key() string { return f.file + ":" + f.fn }

func Run(root string) error {
	repo := capability.RepoForGate(Extension(), root)
	dirs := []string{guardkit.RepoPath(repo, "tools")}
	findings, examined, err := collect(repo, dirs)
	if err != nil {
		return err
	}
	// A guard that walked nothing prints the same green as one that walked
	// everything, and "walked nothing" is what a moved tree looks like.
	if err := guardkit.RequireCorpus("summary-secret-guard", examined, dirs); err != nil {
		return err
	}

	seen := map[string]bool{}
	failed := false
	for _, f := range findings {
		rule, ok := summaryComputedAllowed[f.key()]
		seen[f.key()] = true
		if ok {
			fmt.Printf("  ok: %s:%d %s() computes into the summary — %s\n", f.file, f.line, f.fn, rule.reason)
			continue
		}
		failed = true
		fmt.Printf("::error file=%s,line=%d::%s() calls ghsecret.Mask AND writes a computed value to $GITHUB_STEP_SUMMARY: %s. Masking redacts the LOG stream; a job summary is rendered from a file that masking never touches, and Actions READ is enough to open it. Either append only string literals here, or register %q in summaryComputedAllowed (tools/internal/extensions/guards/summarysecret/guard.go) with a reason naming what the expression evaluates to.\n",
			f.file, f.line, f.fn, f.expr, f.key())
	}

	// Stale entries: the call site is gone, so the line is now misinformation.
	var stale []string
	for k := range summaryComputedAllowed {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		failed = true
		fmt.Printf("::error::summaryComputedAllowed entry %q matches nothing in the tree. The call site was removed or renamed — delete the entry. A registry that keeps dead entries stops being reviewable, because a reader cannot tell which lines are load-bearing.\n", k)
	}

	if failed {
		return fmt.Errorf("summary-secret-guard: unregistered computed job-summary write(s) and/or stale registry entries")
	}
	fmt.Printf("summary-secret-guard: %d file(s) examined, %d computed job-summary write(s) in masking files, all registered.\n", examined, len(findings))
	return nil
}

// guardOwnDir is the path fragment identifying this package's own source. Kept as
// a single constant so the self-exemption and the registry's keys cannot drift
// apart — plaintext-guard's extraction scar: exempting by BASENAME meant moving
// the file out of package main silently re-enabled the guard against itself.
const guardOwnDir = "tools/internal/extensions/guards/summarysecret/"

func collect(repo capability.Repo, dirs []string) ([]finding, int, error) {
	var out []finding
	examined := 0
	for _, dir := range dirs {
		err := repo.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a missing tree is RequireCorpus's problem, not a walk error
			}
			if d.IsDir() {
				if b := d.Name(); b == "vendor" || b == "rendered" || b == "coverage" || b == ".git" || b == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// This guard's own source names the symbols it searches for, in its
			// registry keys and its finding message, so it would report itself.
			if strings.Contains(filepath.ToSlash(path), guardOwnDir) {
				return nil
			}
			b, rerr := repo.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			examined++
			fs, ferr := scan(relForKey(path), string(b))
			if ferr != nil {
				// Unparseable Go is a FAILURE, not a skip: a file this cannot read
				// is a file whose summary writes it cannot see.
				return ferr
			}
			out = append(out, fs...)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, examined, nil
}

// relForKey normalizes a walked path to the repo-relative, slash-separated form
// the registry keys use, whether the guard was pointed at a template checkout or
// at an instance (where the same tree sits under instance-template/).
func relForKey(path string) string {
	p := filepath.ToSlash(path)
	if i := strings.Index(p, "instance-template/"); i >= 0 {
		p = p[i+len("instance-template/"):]
	}
	return strings.TrimPrefix(p, "./")
}

// scan is the pure scanner — file content in, findings out — so the match rules
// are unit-tested without a tree on disk.
func scan(rel, src string) ([]finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, fmt.Errorf("summary-secret-guard: %s did not parse: %w", rel, err)
	}
	if !masksSecrets(file) {
		return nil, nil
	}
	var out []finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		for _, call := range summaryAppends(fn.Body) {
			// args[0] is the "GITHUB_STEP_SUMMARY" selector itself.
			for _, arg := range call.Args[1:] {
				if isStringLiteral(arg) {
					continue
				}
				out = append(out, finding{
					file: rel,
					fn:   funcName(fn),
					line: fset.Position(arg.Pos()).Line,
					expr: exprText(src, fset, arg),
				})
			}
		}
	}
	return out, nil
}

// funcName renders a method as "Recv.Name" so two same-named methods on
// different receivers get distinct registry keys.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// masksSecrets reports whether the file calls ghsecret.Mask anywhere — the
// author's own marker that this code holds material that must not be printed. Matched on the SELECTOR (`.Mask` on an identifier named `ghsecret`)
// rather than on resolved types, which would need a full type-checker load for a
// package whose import path never varies.
func masksSecrets(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Mask" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "ghsecret" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// summaryAppends finds every call whose FIRST argument names the step-summary
// file. Keyed on the argument rather than on `ghaout.Append` so a future wrapper
// with a different name is caught too: what identifies the hazard is the
// destination, not the helper.
func summaryAppends(body *ast.BlockStmt) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok &&
			lit.Kind == token.STRING && strings.Contains(lit.Value, "GITHUB_STEP_SUMMARY") {
			out = append(out, call)
		}
		return true
	})
	return out
}

// isStringLiteral reports whether an expression is a constant string — a bare
// literal, or literals joined with `+`. Anything else is computed, and computed
// is what this guard is about.
func isStringLiteral(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING
	case *ast.ParenExpr:
		return isStringLiteral(v.X)
	case *ast.BinaryExpr:
		return v.Op == token.ADD && isStringLiteral(v.X) && isStringLiteral(v.Y)
	}
	return false
}

// exprText recovers the source text of an expression for the finding message —
// a reviewer needs to see WHICH expression, and an AST dump is unreadable.
func exprText(src string, fset *token.FileSet, e ast.Expr) string {
	start, end := fset.Position(e.Pos()).Offset, fset.Position(e.End()).Offset
	if start < 0 || end > len(src) || start >= end {
		return "<expression>"
	}
	t := strings.Join(strings.Fields(src[start:end]), " ")
	if len(t) > 80 {
		t = t[:77] + "..."
	}
	return t
}
