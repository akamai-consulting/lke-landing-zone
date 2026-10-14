package linode

// acl_mutation_test.go covers the two ACL helpers that were only ever exercised
// INDIRECTLY, through WithIP and the PUT body — a route that cannot see either
// of the properties they exist for. NonNil's whole job is the nil case, and the
// only PUT test passes a non-nil slice. collapseAdjacentInPlace's `i == 0` guard
// only matters when the first element equals the zero value it compares against,
// which no IPv4 address ever does.

import (
	"reflect"
	"testing"
)

// TestNonNil pins BOTH directions: a nil slice must become an empty non-nil one
// (so the Linode ACL API sees [] and not null), and a populated slice must come
// back untouched.
func TestNonNil(t *testing.T) {
	got := NonNil(nil)
	if got == nil {
		t.Error("NonNil(nil) returned nil — it would marshal as null, which the ACL API rejects")
	}
	if len(got) != 0 {
		t.Errorf("NonNil(nil) = %v, want an empty slice", got)
	}
	in := []string{"1.1.1.1/32", "9.9.9.0/24"}
	if out := NonNil(in); !reflect.DeepEqual(out, in) {
		t.Errorf("NonNil(%v) = %v, want the input unchanged", in, out)
	}
	// An already-empty non-nil slice is passed through, not reallocated away.
	if out := NonNil([]string{}); out == nil || len(out) != 0 {
		t.Errorf("NonNil([]) = %v, want an empty non-nil slice", out)
	}
}

// TestCollapseAdjacentInPlace exercises the dedup loop's two independent
// conditions. The empty first element is the load-bearing case: `prev` starts at
// "", so without the `i == 0` term the first entry of a slice that begins with
// "" would be dropped as a duplicate of nothing.
func TestCollapseAdjacentInPlace(t *testing.T) {
	cases := []struct {
		in, want []string
		why      string
	}{
		{nil, []string{}, "nil input"},
		{[]string{}, []string{}, "empty input"},
		{[]string{"a"}, []string{"a"}, "single element"},
		{[]string{"", "a"}, []string{"", "a"}, "leading empty string is kept — i == 0, not a dup of prev's zero value"},
		{[]string{"", "", "a"}, []string{"", "a"}, "repeated empty strings still collapse to one"},
		{[]string{"a", "a", "b"}, []string{"a", "b"}, "adjacent duplicates collapse"},
		{[]string{"a", "b", "a"}, []string{"a", "b", "a"}, "non-adjacent duplicates are NOT collapsed (sorted-input precondition)"},
		{[]string{"a", "a", "a"}, []string{"a"}, "a run of duplicates collapses to one"},
	}
	for _, c := range cases {
		got := collapseAdjacentInPlace(append([]string{}, c.in...))
		if len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("collapseAdjacentInPlace(%q) = %q, want %q (%s)", c.in, got, c.want, c.why)
		}
	}
}
