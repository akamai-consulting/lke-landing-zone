package main

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/instancelayout"
)

// setHCLField rewrites EVERY matching `^<key> =` line, not just the first. That is
// safe only while no embedded terraform.tfvars.example declares the same key twice
// at column 0 — two would both be rewritten to the same assignment, and HCL rejects
// a redefined attribute, so `llz render` would emit a tfvars that cannot be parsed.
//
// Nothing else enforces that invariant, and the examples are hand-edited prose-heavy
// files where a duplicate is easy to introduce (the databases example alone carries
// a 30-line commented block). Assert it rather than trusting it.
func TestTfrootExamples_NoDuplicateTopLevelKeys(t *testing.T) {
	for _, root := range instancelayout.Roots {
		base, err := tfrootExample(root)
		if err != nil {
			t.Errorf("%s: %v", root, err)
			continue
		}
		seen := map[string]int{}
		for _, line := range strings.Split(base, "\n") {
			// Column 0 only, matching hasHCLKey/setHCLField: a leading space or '#'
			// is a comment or a nested attribute, neither of which they touch.
			if line == "" || line[0] == '#' || line[0] == ' ' || line[0] == '\t' {
				continue
			}
			i := strings.Index(line, "=")
			if i <= 0 {
				continue
			}
			k := strings.TrimSpace(line[:i])
			if k != "" && !strings.ContainsAny(k, " \t") {
				seen[k]++
			}
		}
		for k, n := range seen {
			if n > 1 {
				t.Errorf("%s/terraform.tfvars.example declares %q %d times at column 0 — "+
					"setHCLField rewrites every match, so render would emit a duplicate "+
					"assignment and HCL would reject it as a redefined attribute", root, k, n)
			}
		}
	}
}
