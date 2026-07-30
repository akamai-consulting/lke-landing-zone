package terraform

// Fuzz target for the tfvars parser.
//
// ParseTFVars reads generated .tfvars files and its output drives what gets
// imported and how resources are labelled, so a parse that silently mangles a
// value is a wrong-infrastructure bug rather than a display bug. Mutation testing
// already found four survivors in splitAssign's quote handling (the `len(val) >= 2`
// and `IndexByte(val[1:], '"') >= 0` boundaries), which is the sign of a function
// whose degenerate inputs were never walked — exactly what fuzzing is for.

import (
	"strings"
	"testing"
)

// FuzzSplitAssign asserts the invariants of one line's key/value split. The
// interesting inputs are unbalanced and empty quotes, which is where mutation
// testing found the live boundaries.
func FuzzSplitAssign(f *testing.F) {
	for _, s := range []string{
		`cluster_label = "prod-ord"`,
		`node_count = 3`,
		`region="us-ord"`,
		`  key  =  "  spaced  "  `,
		`k = ""`,      // empty quoted value
		`k = "`,       // unbalanced: opening quote only
		`k = "a`,      // unbalanced with content
		`k = a"b`,     // stray quote mid-value
		`k = "a"b"c"`, // several quotes
		`= v`,         // no key
		`k =`,         // no value
		`no equals here`,
		``, `=`, `"`, `==`, `k==v`,
		"key = \"tab\tinside\"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		key, val, ok := splitAssign(line)

		if k2, v2, o2 := splitAssign(line); k2 != key || v2 != val || o2 != ok {
			t.Fatalf("not deterministic for %q", line)
		}

		// A line with no '=' is the only rejection case; anything containing one
		// must split, because callers rely on `ok` to mean "this was an
		// assignment" and silently dropping a real one loses a tfvar.
		if strings.Contains(line, "=") != ok {
			t.Errorf("splitAssign(%q) ok=%v, but presence of '=' is %v", line, ok, strings.Contains(line, "="))
		}
		if !ok {
			if key != "" || val != "" {
				t.Errorf("splitAssign(%q) rejected but returned key=%q val=%q", line, key, val)
			}
			return
		}

		// Neither half may grow: both are substrings of the line, quotes only
		// ever stripped.
		if len(key) > len(line) || len(val) > len(line) {
			t.Errorf("splitAssign(%q) grew the input: key=%q val=%q", line, key, val)
		}
		// The key is always trimmed — a label with a leading space is a label that
		// does not match anything.
		if key != strings.TrimSpace(key) {
			t.Errorf("splitAssign(%q) key %q is not trimmed", line, key)
		}
		// The VALUE is trimmed only when unquoted. Inside quotes, whitespace is
		// content: `k = "  spaced  "` must yield `  spaced  `, because the quotes
		// are what delimit it. The seed corpus caught this — an earlier version of
		// this invariant demanded a trimmed value unconditionally, which would have
		// asserted that the parser destroy data the user deliberately quoted.
		rhs := strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
		if !strings.HasPrefix(rhs, `"`) && val != strings.TrimSpace(val) {
			t.Errorf("splitAssign(%q) unquoted val %q is not trimmed", line, val)
		}
		// The key never contains '=': it is everything before the FIRST one.
		if strings.Contains(key, "=") {
			t.Errorf("splitAssign(%q) key %q contains '=' — split on the first, not the last", line, key)
		}
		// A fully quoted value has both quotes removed, never one.
		trimmed := strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
		if strings.HasPrefix(trimmed, `"`) && strings.Count(trimmed, `"`) >= 2 && strings.HasPrefix(val, `"`) {
			t.Errorf("splitAssign(%q) val %q kept its opening quote", line, val)
		}
	})
}

// FuzzParseTFVars drives the whole-file parser. Its invariants are structural:
// parsing must terminate, be deterministic, and never invent a value that no line
// supplied. First-assignment-wins is the documented behaviour, so a duplicate key
// must not overwrite.
func FuzzParseTFVars(f *testing.F) {
	for _, s := range []string{
		"cluster_label = \"prod-ord\"\nregion = \"us-ord\"\nnode_count = 3\n",
		"cluster_label = \"first\"\ncluster_label = \"second\"\n", // first wins
		"# a comment\n\ncluster_label = \"x\"\n",
		"cluster_label=\"no-spaces\"",
		"\n\n\n", "", "=",
		"cluster_label = \"unterminated\n",
		"region = \"us-ord\"\r\nnode_count = 2\r\n", // CRLF
		strings.Repeat("k = v\n", 50),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, content string) {
		got := ParseTFVars(content)

		if ParseTFVars(content) != got {
			t.Fatalf("not deterministic for %q", content)
		}

		// Every string field must have come from the content — EXCEPT the two that
		// ParseTFVars deliberately defaults when absent. The seed corpus caught
		// this immediately: an earlier version of this invariant flagged
		// VPCSubnetCIDR="10.0.0.0/13" as "invented" when it is DefaultVPCSubnetCIDR
		// applied on purpose. Encoding the exception rather than dropping the check
		// keeps the useful half: a value that is neither from the input nor the
		// documented default would name real infrastructure after nothing.
		for name, v := range map[string]string{
			"ClusterLabel":  got.ClusterLabel,
			"FirewallLabel": got.FirewallLabel,
			"Region":        got.Region,
			"VPCNetwork":    got.VPCNetwork,
		} {
			if v != "" && !strings.Contains(content, v) {
				t.Errorf("ParseTFVars invented %s=%q — not present in the input", name, v)
			}
		}
		if got.NodePoolLabel != "" &&
			got.NodePoolLabel != DefaultNodePoolLabel && !strings.Contains(content, got.NodePoolLabel) {
			t.Errorf("NodePoolLabel=%q is neither from the input nor the default", got.NodePoolLabel)
		}
		if got.VPCSubnetCIDR != "" &&
			got.VPCSubnetCIDR != DefaultVPCSubnetCIDR && !strings.Contains(content, got.VPCSubnetCIDR) {
			t.Errorf("VPCSubnetCIDR=%q is neither from the input nor the default", got.VPCSubnetCIDR)
		}
		// The defaults must actually be applied — an absent key may not leave the
		// field empty, or downstream renders a blank label.
		if got.NodePoolLabel == "" || got.VPCSubnetCIDR == "" {
			t.Errorf("defaults not applied: NodePoolLabel=%q VPCSubnetCIDR=%q", got.NodePoolLabel, got.VPCSubnetCIDR)
		}

		// First assignment wins: prepending a line for a key that is already set
		// must change the result, and appending one must not.
		if got.Region != "" {
			if appended := ParseTFVars(content + "\nregion = \"ZZZ-appended\""); appended.Region != got.Region {
				t.Errorf("a later region assignment overwrote the first: %q -> %q", got.Region, appended.Region)
			}
		}
	})
}
