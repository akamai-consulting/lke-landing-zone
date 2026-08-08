package environments

import "testing"

// Tests that followed their subjects. first3 and quote are one-liners in this
// package and have NO production caller in package main — so the tests moved
// rather than the symbols being exported, which is the check this campaign
// applies before accepting any export.

func TestFirst3(t *testing.T) {
	cases := map[string]string{"abcdef": "abc", "ab": "ab", "": "", "abc": "abc"}
	for in, want := range cases {
		if got := first3(in); got != want {
			t.Errorf("first3(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuote(t *testing.T) {
	if got := quote("x"); got != `"x"` {
		t.Errorf("quote = %q, want \"x\"", got)
	}
}
