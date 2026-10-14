package cigate

import "testing"

// SplitCSVList arrived here from ci_assert_scrape.go with twelve callers and no
// test of its own — it had only ever been exercised through the verbs that use it.
// The blank-dropping is the part worth pinning: an env var set to "a,,b" or left
// with a trailing comma is the normal shape of a hand-edited workflow input, and a
// caller that received an empty element would go looking for a resource named "".
func TestSplitCSVList(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b", []string{"a", "b"}},
		{"a,", []string{"a"}},
		{",", nil},
		{"", nil},
		{"   ", nil},
	} {
		got := SplitCSVList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("SplitCSVList(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("SplitCSVList(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
