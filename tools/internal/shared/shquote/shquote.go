// Package shquote splits a line of shell into what the shell would treat as
// SYNTAX and what it would treat as literal TEXT.
//
// One character decides a lot: `echo "a; b"` prints a semicolon while `echo a; b`
// runs a second command, and `#` inside quotes is not a comment. Two gates need
// that distinction and neither is about the other's subject — the Makefile-recipe
// counter classifies a line as documentation only if nothing but the command word
// survives quote-stripping, and `llz ci at-rest-guard` strips comments from
// Terraform without being fooled by a `#` inside a string. Sharing one
// implementation is what keeps them agreeing about what a quote is.
package shquote

import "strings"

// StripSpans removes '…' and "…" spans from a shell line, returning the
// unquoted remainder and the concatenated quoted content. Splitting them is what
// lets the caller apply different rules to each: an operator inside quotes is
// literal text (`echo "a; b"` prints a semicolon), while the same character
// outside them is shell syntax.
//
// Backslash escapes are honoured inside double quotes only, matching the shell.
func StripSpans(s string) (rest, quoted string) {
	var out, q strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			for i++; i < len(s) && s[i] != '\''; i++ {
				q.WriteByte(s[i])
			}
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					q.WriteByte(s[i])
					i++
				}
				q.WriteByte(s[i])
			}
		default:
			out.WriteByte(c)
		}
	}
	return strings.TrimSpace(out.String()), q.String()
}
