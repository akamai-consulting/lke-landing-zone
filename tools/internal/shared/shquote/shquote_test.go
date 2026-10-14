package shquote

import "testing"

// stripQuotedSpans is the half that makes the rule safe rather than merely
// convenient: an operator inside quotes is text, the same character outside them
// is syntax, and conflating the two either floods the tally or opens a hole.
func TestStripSpans(t *testing.T) {
	tests := []struct{ in, rest, quoted string }{
		{`echo "a; b"`, "echo", "a; b"},
		{`echo "a" > f`, "echo  > f", "a"},
		{`echo 'x' "y"`, "echo", "xy"},
		{`echo "esc \" inside"`, "echo", `esc \" inside`},
		{`plain words`, "plain words", ""},
		// An unterminated quote must not swallow the caller — it consumes to end
		// of line, which is also what the shell would report an error about.
		{`echo "unterminated`, "echo", "unterminated"},
	}
	for _, tt := range tests {
		rest, quoted := StripSpans(tt.in)
		if rest != tt.rest || quoted != tt.quoted {
			t.Errorf("StripSpans(%q) = (%q, %q), want (%q, %q)", tt.in, rest, quoted, tt.rest, tt.quoted)
		}
	}
}
