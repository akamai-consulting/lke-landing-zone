package terraform

// Parse-boundary coverage for the tfvars reader. The happy paths are covered by
// TestParseTFVars; what was untested is the degenerate input a hand-edited
// <region>.tfvars actually produces — an assignment with no key, an empty
// quoted value, an unterminated quote — and the exact cluster-label truncation
// boundary the firewall label derives from.

import (
	"strings"
	"testing"
)

// splitAssign is the whole parser's boundary logic: where the '=' is, whether
// the value is quoted, and where the closing quote is. ParseTFVars discards
// unknown keys, so these cases are only observable on the helper itself.
func TestSplitAssignBoundaries(t *testing.T) {
	cases := []struct {
		name string
		line string
		key  string
		val  string
		ok   bool
	}{
		{"plain assignment", `cluster_label = "c1"`, "cluster_label", "c1", true},
		{"no equals", "cluster_label", "", "", false},
		{"empty line", "", "", "", false},
		// '=' at index 0: there IS an assignment (ok), it just has an empty key —
		// distinct from "no '=' on the line at all".
		{"equals first", `= "orphan"`, "", "orphan", true},
		// Both quotes present with nothing between them: the value is the empty
		// string, not a literal `""`.
		{"empty quoted value", `cluster_label = ""`, "cluster_label", "", true},
		// A lone quote is too short to be a quoted string; it survives verbatim
		// rather than being sliced.
		{"lone quote", `cluster_label = "`, "cluster_label", `"`, true},
		{"unterminated quote", `cluster_label = "abc`, "cluster_label", `"abc`, true},
		{"unquoted value", "node_count = 3", "node_count", "3", true},
		{"trailing comment", `cluster_label = "c1" # why`, "cluster_label", "c1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, val, ok := splitAssign(c.line)
			if ok != c.ok || key != c.key || val != c.val {
				t.Errorf("splitAssign(%q) = (%q,%q,%v), want (%q,%q,%v)",
					c.line, key, val, ok, c.key, c.val, c.ok)
			}
		})
	}
}

// An explicitly empty value must parse as empty — i.e. the quotes are stripped
// even when they surround nothing. If the `""` were carried through verbatim the
// value would be a non-empty two-character string, defeating every downstream
// `== ""` fallback (node_pool_label would stop defaulting, and a cluster label of
// `""` would be handed to the Linode API).
func TestParseTFVarsEmptyQuotedValue(t *testing.T) {
	v := ParseTFVars("cluster_label = \"\"\nnode_pool_label = \"\"\nnode_type = \"\"\n")
	if v.ClusterLabel != "" {
		t.Errorf("ClusterLabel = %q, want the empty string (quotes stripped)", v.ClusterLabel)
	}
	if v.NodeType != "" {
		t.Errorf("NodeType = %q, want the empty string (quotes stripped)", v.NodeType)
	}
	if v.NodePoolLabel != DefaultNodePoolLabel {
		t.Errorf("NodePoolLabel = %q, want the default %q (an empty value must still default)",
			v.NodePoolLabel, DefaultNodePoolLabel)
	}
}

// The firewall label truncates cluster_label at exactly clusterLabelTrunc chars,
// matching substr(cluster_label,0,26) in llz-cluster/main.tf. A label of exactly
// that length must pass through whole; one char more must lose exactly one char.
// An off-by-one here re-creates the orphan-firewall/apply-collision class the
// retired import script had.
func TestResolveFirewallLabelTruncationBoundary(t *testing.T) {
	exact := strings.Repeat("a", clusterLabelTrunc)
	if got, want := ResolveFirewallLabel(TFVars{ClusterLabel: exact}), exact+"-nodes"; got != want {
		t.Errorf("exactly %d chars: %q, want %q (no truncation at the boundary)", clusterLabelTrunc, got, want)
	}
	if got, want := ResolveFirewallLabel(TFVars{ClusterLabel: exact + "b"}), exact+"-nodes"; got != want {
		t.Errorf("%d chars: %q, want %q", clusterLabelTrunc+1, got, want)
	}
	// One char under the bound is untouched too (guards against truncating short
	// labels, which would also panic-slice a label shorter than the bound).
	short := strings.Repeat("a", clusterLabelTrunc-1)
	if got, want := ResolveFirewallLabel(TFVars{ClusterLabel: short}), short+"-nodes"; got != want {
		t.Errorf("%d chars: %q, want %q", clusterLabelTrunc-1, got, want)
	}
}
