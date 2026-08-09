// Package tfvars reads and writes single-line HCL assignments in a .tfvars file.
//
// THE THREE PARTS WERE IN THREE DIFFERENT FILES: the reader in
// ci_seed_special.go, the writer in scaffold.go, the existence check in
// render.go. Nothing named them as one thing, and putting them side by side
// surfaces a disagreement none of the three files could show on its own:
//
//	Value    accepts an INDENTED assignment — it splits on '=' and TrimSpace()s
//	         the left side, so "  key = x" is a match.
//	SetField anchors at column 0 (`(?m)^key\s*=`).
//	HasKey   anchors at column 0 (CutPrefix on the raw line).
//
// So `llz` can READ a value it would never REWRITE. It has not bitten anyone,
// because this tree generates these files and generates them unindented — but it
// is exactly the kind of thing that holds until the first hand-edited tfvars
// arrives. Pinned by TestReadAndWriteAgreeOnWhatAnAssignmentIs, which is
// deliberately written to FAIL if someone "fixes" one side without the other.
//
// All three are strictly LINE-ORIENTED and that is a deliberate limit, not an
// oversight: these files are generated from a template and the only edits llz
// makes are whole-line replacements. A real HCL parser would be correct about
// heredocs and multi-line lists that this tree never writes, at the cost of a
// dependency and a round-trip that reformats everything a human put there.
package tfvars

import (
	"regexp"
	"strings"
)

// Value returns the first `key = "value"` assignment in tfvars content
// (quotes stripped, comments ignored) — the same first-wins grep/sed
// semantics as internal/terraform.ParseTFVars, for keys outside its fixed
// struct (obj_cluster).
func Value(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		i := strings.IndexByte(line, '=')
		if i < 0 || strings.TrimSpace(line[:i]) != key {
			continue
		}
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 && val[0] == '"' {
			if j := strings.IndexByte(val[1:], '"'); j >= 0 {
				return val[1 : 1+j]
			}
		}
		return val
	}
	return ""
}

// SetField replaces EVERY `^<key> ... = ...` line with `<key> = <value>` (the
// doc said "the first" for years; the call has always been a ReplaceAll). Harmless
// today — no embedded terraform.tfvars.example declares the same key twice at
// column 0, which a test asserts — but two would both be rewritten into a
// duplicate assignment, which HCL rejects as a redefined attribute.
// Matches the bash `replace_in_file "^<key> .*=.*"` line-rewrite.
//
// ReplaceAllLiteral, NOT ReplaceAllString: the replacement is a rendered HCL value,
// and ReplaceAllString EXPANDS `$name`/`${name}` in it. Every value here comes from
// the spec, so a `$` in one was silently eaten rather than written — a Linode tag
// `cost$1center` rendered as `"cost"`, and `owner${team}` as `"owner"`, tagging real
// infrastructure wrong with no error (spec.cluster.tags is free-form, so nothing
// upstream rejects it). No caller wants expansion; these are literals.
func SetField(content, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=.*$`)
	return re.ReplaceAllLiteralString(content, key+" = "+value)
}

// HasKey reports whether content has an uncommented `<key> =` assignment: a line
// STARTING with key (so a commented-out `# key = …` does not match) followed by
// optional blanks and `=`. A line scan rather than a regexp — this runs once per
// assignment × per root × per env on both the write and the --diff path, and the
// old `regexp.MustCompile` on every call recompiled the pattern every time.
func HasKey(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		rest, ok := strings.CutPrefix(line, key)
		if ok && strings.HasPrefix(strings.TrimLeft(rest, " \t"), "=") {
			return true
		}
	}
	return false
}
